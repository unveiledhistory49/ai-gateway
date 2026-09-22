package dispatcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/company/ai-gateway/internal/metrics"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/policy"
	"github.com/company/ai-gateway/internal/resilience"
	"github.com/company/ai-gateway/internal/tracing"
)

// bufferPool recycles 4KB byte slices for zero-copy streaming passes.
var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// Stage implements Stage 3: Upstream Dispatcher with Resilience & Health Engine.
type Stage struct {
	client   *http.Client
	registry *resilience.Registry
	retryCfg resilience.RetryConfig
	logger   *slog.Logger
	metrics  *metrics.Metrics
}

// NewStage constructs a new Upstream Dispatcher stage with a tuned persistent HTTP client pool.
func NewStage(defaultTimeout time.Duration, registry ...*resilience.Registry) *Stage {
	if defaultTimeout <= 0 {
		defaultTimeout = 30 * time.Second
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,  // Connect timeout from FAILURE-MODES.md
			KeepAlive: 30 * time.Second, // TCP keepalive
		}).DialContext,
		MaxIdleConns:          10000,
		MaxIdleConnsPerHost:   500,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: defaultTimeout, // TTFT header deadline
		ForceAttemptHTTP2:     true,
	}

	var reg *resilience.Registry
	if len(registry) > 0 {
		reg = registry[0]
	}

	retryCfg := resilience.RetryConfig{}
	retryCfg.ApplyDefaults()

	return &Stage{
		client: &http.Client{
			Transport: transport,
		},
		registry: reg,
		retryCfg: retryCfg,
		logger:   slog.Default(),
		metrics:  metrics.Default(),
	}
}

// NewStageWithClient allows injecting a custom HTTP client (e.g., for unit/integration testing).
func NewStageWithClient(client *http.Client, registry ...*resilience.Registry) *Stage {
	var reg *resilience.Registry
	if len(registry) > 0 {
		reg = registry[0]
	}

	retryCfg := resilience.RetryConfig{}
	retryCfg.ApplyDefaults()

	return &Stage{
		client:   client,
		registry: reg,
		retryCfg: retryCfg,
		logger:   slog.Default(),
		metrics:  metrics.Default(),
	}
}

// NewStageWithConfig constructs a Stage with full configuration injection.
func NewStageWithConfig(client *http.Client, registry *resilience.Registry, retryCfg resilience.RetryConfig, logger *slog.Logger) *Stage {
	retryCfg.ApplyDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	return &Stage{
		client:   client,
		registry: registry,
		retryCfg: retryCfg,
		logger:   logger,
		metrics:  metrics.Default(),
	}
}

// SetMetrics updates the metrics engine for Stage.
func (s *Stage) SetMetrics(m *metrics.Metrics) {
	s.metrics = m
}

// Name returns the identifier of this stage.
func (s *Stage) Name() string {
	return "upstream_dispatcher"
}

// Execute handles forwarding the request to the upstream provider, executing adaptive retries
// and failover cascades across healthy priority targets.
func (s *Stage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.ChatRequest == nil {
		return model.NewGatewayError(
			http.StatusBadRequest,
			model.ErrCodeBadRequest,
			"Cannot dispatch request: missing chat request.",
		)
	}

	// Resolve cascade targets
	targets := reqCtx.Targets
	if len(targets) == 0 {
		if reqCtx.Upstream == nil {
			return model.NewGatewayError(
				http.StatusInternalServerError,
				model.ErrCodeInternalError,
				"Cannot dispatch request: missing route or upstream context.",
			)
		}
		// Backward compatibility: build single target from Upstream and TargetModel
		targets = []*model.RouteTarget{
			{
				UpstreamID:   reqCtx.Upstream.ID,
				Provider:     reqCtx.Upstream.Provider,
				EndpointURL:  reqCtx.Upstream.EndpointURL,
				TargetModel:  reqCtx.TargetModel,
				APIKey:       reqCtx.Upstream.APIKey,
				Timeout:      reqCtx.Upstream.Timeout(),
				PriorityTier: 0,
				Weight:       100,
			},
		}
	}

	var lastErr error

	// Cascade through targets: Primary (Tier 0) -> Fallback (Tier 1) -> Emergency (Tier 2)
	for targetIdx, target := range targets {
		if reqCtx.Context.Err() != nil {
			return reqCtx.Context.Err()
		}

		// Check circuit breaker status
		var cb *resilience.CircuitBreaker
		if s.registry != nil {
			cb = s.registry.Get(target.UpstreamID)
			if !cb.Allow() {
				// Target's breaker is OPEN, skip to next fallback target in cascade
				continue
			}
		}

		reqCtx.TargetModel = target.TargetModel
		hasNextTarget := targetIdx < len(targets)-1

		// Prepare upstream payload with target model mapping
		upstreamReqPayload := *reqCtx.ChatRequest
		upstreamReqPayload.Model = target.TargetModel

		payloadBytes, err := json.Marshal(upstreamReqPayload)
		if err != nil {
			return model.NewGatewayError(
				http.StatusInternalServerError,
				model.ErrCodeInternalError,
				fmt.Sprintf("Failed to serialize upstream payload: %v", err),
			)
		}

		endpointURL := strings.TrimRight(target.EndpointURL, "/") + "/v1/chat/completions"

		// If there is another fallback target in the cascade, we attempt the current target once
		// and failover immediately if it fails with retriable/connection error.
		// If this is the last available target, we retry up to MaxRetries using Full Jitter backoff.
		maxAttempts := 0
		if !hasNextTarget {
			maxAttempts = s.retryCfg.MaxRetries
		}

		for attempt := 0; attempt <= maxAttempts; attempt++ {
			if reqCtx.Context.Err() != nil {
				return reqCtx.Context.Err()
			}

			upstreamCtx, cancel := context.WithCancel(reqCtx.Context)
			if !reqCtx.ChatRequest.Stream && target.Timeout > 0 {
				var timeoutCancel context.CancelFunc
				upstreamCtx, timeoutCancel = context.WithTimeout(upstreamCtx, target.Timeout)
				defer timeoutCancel()
			}

			httpReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpointURL, bytes.NewReader(payloadBytes))
			if err != nil {
				cancel()
				return model.NewGatewayError(
					http.StatusInternalServerError,
					model.ErrCodeInternalError,
					fmt.Sprintf("Failed to construct upstream request: %v", err),
				)
			}

			httpReq.Header.Set("Content-Type", "application/json")
			if reqCtx.ChatRequest.Stream {
				httpReq.Header.Set("Accept", "text/event-stream")
			} else {
				httpReq.Header.Set("Accept", "application/json")
			}

			if target.APIKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+target.APIKey)
			}

			// Distributed Tracing: inject traceparent and X-Request-ID
			if reqCtx.TraceID != "" {
				childSpanID := tracing.NewSpanID()
				httpReq.Header.Set(tracing.TraceparentHeader, tracing.FormatTraceparent(reqCtx.TraceID, childSpanID))
			}
			if reqCtx.RequestID != "" {
				httpReq.Header.Set(tracing.RequestIDHeader, reqCtx.RequestID)
			}

			reqCtx.UpstreamID = target.UpstreamID
			reqCtx.UpstreamModel = target.TargetModel
			upstreamStart := time.Now()
			resp, err := s.client.Do(httpReq)
			callDuration := time.Since(upstreamStart)
			reqCtx.UpstreamDuration += callDuration

			statusCodeStr := "0"
			if resp != nil {
				statusCodeStr = strconv.Itoa(resp.StatusCode)
			}
			if s.metrics != nil {
				s.metrics.RecordUpstream(target.UpstreamID, target.TargetModel, statusCodeStr, callDuration.Seconds())
			}
			if err != nil {
				cancel()
				if reqCtx.Context.Err() != nil {
					return reqCtx.Context.Err()
				}

				if cb != nil {
					cb.RecordFailure()
				}

				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					lastErr = model.NewGatewayError(
						http.StatusGatewayTimeout,
						model.ErrCodeUpstreamTimeout,
						fmt.Sprintf("Upstream provider '%s' timed out: %v", target.UpstreamID, err),
					)
				} else {
					lastErr = model.NewGatewayError(
						http.StatusBadGateway,
						model.ErrCodeUpstreamBadGateway,
						fmt.Sprintf("Failed to reach upstream provider '%s': %v", target.UpstreamID, err),
					)
				}

				if resilience.IsRetryableNetworkError(err) {
					if hasNextTarget {
						// Per FAILURE-MODES.md: "Zero-delay switch to alternative endpoint; bypass exponential backoff; mark upstream host unhealthy."
						s.logger.Info("Failing over to next target due to transport error",
							"from_upstream", target.UpstreamID,
							"error", err.Error(),
						)
						break
					}
					// Retry same target if attempts remain
					if attempt < maxAttempts {
						sleepDur := resilience.FullJitterBackoff(attempt, s.retryCfg.BaseDelay, s.retryCfg.MaxDelay, s.retryCfg.UniformRand)
						select {
						case <-time.After(sleepDur):
						case <-reqCtx.Context.Done():
							return reqCtx.Context.Err()
						}
						continue
					}
				}
				return lastErr
			}

			// 2xx Success
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if cb != nil {
					cb.RecordSuccess()
				}

				if reqCtx.ChatRequest.Stream {
					defer cancel()
					// CRUCIAL SAFETY RULE:
					// Once chunks are flushed downstream, retries are prohibited!
					return s.handleStreamingResponse(upstreamCtx, resp, reqCtx, cb)
				}

				defer resp.Body.Close()
				cancel()
				return s.handleSyncResponse(resp, reqCtx)
			}

			// Upstream returned non-2xx error status code
			lastErr = s.handleUpstreamError(resp, target.UpstreamID)
			resp.Body.Close()
			cancel()

			// Non-retryable errors (400, 401, 403, 404, 422) return immediately
			if resilience.IsNonRetryableStatus(resp.StatusCode) {
				return lastErr
			}

			// Retriable errors: record failure on target's circuit breaker
			if cb != nil {
				cb.RecordFailure()
			}

			// Check Retry-After header for 429
			retryAfterDur, hasRetryAfter, exceedsThreshold := resilience.ParseRetryAfter(resp.Header, time.Now())
			if hasRetryAfter && exceedsThreshold && hasNextTarget {
				s.logger.Info("Retry-After exceeds 5s threshold, failing over to next target",
					"from_upstream", target.UpstreamID,
					"retry_after_seconds", retryAfterDur.Seconds(),
				)
				break
			}

			if hasNextTarget {
				s.logger.Info("Failing over to next target in cascade due to retriable status",
					"from_upstream", target.UpstreamID,
					"status", resp.StatusCode,
				)
				break
			}

			// If retrying same target:
			if attempt < maxAttempts {
				var sleepDur time.Duration
				if hasRetryAfter && retryAfterDur > 0 {
					sleepDur = retryAfterDur
				} else {
					sleepDur = resilience.FullJitterBackoff(attempt, s.retryCfg.BaseDelay, s.retryCfg.MaxDelay, s.retryCfg.UniformRand)
				}
				select {
				case <-time.After(sleepDur):
				case <-reqCtx.Context.Done():
					return reqCtx.Context.Err()
				}
				continue
			}
		}
	}

	if lastErr != nil {
		return lastErr
	}

	return model.NewGatewayError(
		http.StatusServiceUnavailable,
		model.ErrCodeCircuitOpen,
		"All upstreams unavailable.",
	)
}

// handleUpstreamError translates upstream error responses into structured GatewayError.
func (s *Stage) handleUpstreamError(resp *http.Response, upstreamID string) error {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	msg := string(bodyBytes)

	// Attempt to extract standard OpenAI error message if present
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bodyBytes, &parsed); err == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}

	switch resp.StatusCode {
	case http.StatusBadRequest:
		return model.NewGatewayError(http.StatusBadRequest, model.ErrCodeBadRequest, fmt.Sprintf("Upstream provider '%s' returned 400 Bad Request: %s", upstreamID, msg))
	case http.StatusUnauthorized:
		return model.NewGatewayError(http.StatusUnauthorized, model.ErrCodeMissingOrInvalidAPIKey, fmt.Sprintf("Upstream provider '%s' returned 401 Unauthorized: %s", upstreamID, msg))
	case http.StatusForbidden:
		return model.NewGatewayError(http.StatusForbidden, model.ErrCodeForbiddenRoute, fmt.Sprintf("Upstream provider '%s' returned 403 Forbidden: %s", upstreamID, msg))
	case http.StatusNotFound:
		return model.NewGatewayError(http.StatusNotFound, model.ErrCodeUnknownRoute, fmt.Sprintf("Upstream provider '%s' returned 404 Not Found: %s", upstreamID, msg))
	case http.StatusBadGateway:
		return model.NewGatewayError(http.StatusBadGateway, model.ErrCodeUpstreamBadGateway, fmt.Sprintf("Upstream provider '%s' returned 502 Bad Gateway: %s", upstreamID, msg))
	case http.StatusServiceUnavailable:
		return model.NewGatewayError(http.StatusServiceUnavailable, model.ErrCodeUpstreamUnavailable, fmt.Sprintf("Upstream provider '%s' returned 503 Service Unavailable: %s", upstreamID, msg))
	case http.StatusGatewayTimeout:
		return model.NewGatewayError(http.StatusGatewayTimeout, model.ErrCodeUpstreamTimeout, fmt.Sprintf("Upstream provider '%s' returned 504 Gateway Timeout: %s", upstreamID, msg))
	case http.StatusTooManyRequests:
		return model.NewGatewayError(http.StatusTooManyRequests, model.ErrCodeRateLimitExceeded, fmt.Sprintf("Upstream provider '%s' rate limited: %s", upstreamID, msg))
	default:
		return model.NewGatewayError(resp.StatusCode, "UPSTREAM_ERROR", fmt.Sprintf("Upstream provider '%s' error (HTTP %d): %s", upstreamID, resp.StatusCode, msg))
	}
}

// handleSyncResponse buffers and delivers a non-streaming JSON completion response.
func (s *Stage) handleSyncResponse(resp *http.Response, reqCtx *pipeline.RequestContext) error {
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		if reqCtx.Context.Err() != nil {
			return reqCtx.Context.Err()
		}
		return model.NewGatewayError(
			http.StatusBadGateway,
			model.ErrCodeUpstreamBadGateway,
			fmt.Sprintf("Failed to read response from upstream: %v", err),
		)
	}

	var chatResp model.CanonicalChatResponse
	if err := json.Unmarshal(bodyBytes, &chatResp); err == nil {
		reqCtx.ChatResponse = &chatResp
	}

	if reqCtx.SkipSyncWrite {
		// Response delivery and token reconciliation deferred to PostPolicyStage
		return nil
	}

	// Direct write when SkipSyncWrite is false (e.g., isolated dispatcher unit tests)
	reqCtx.Writer.Header().Set("Content-Type", "application/json")
	reqCtx.Writer.WriteHeader(resp.StatusCode)
	reqCtx.Writer.Write(bodyBytes)

	// Reconcile tokens if callback present
	if reqCtx.ChatResponse != nil && reqCtx.ChatResponse.Usage != nil {
		reqCtx.ExecuteReconcile(reqCtx.ChatResponse.Usage.TotalTokens)
	} else {
		reqCtx.ExecuteReconcile(0)
	}

	reqCtx.ShortCircuited = true
	return nil
}

// handleStreamingResponse streams SSE chunks incrementally with active upstream socket teardown
// and streaming lookahead buffer for DLP policy enforcement.
// CRUCIAL SAFETY RULE:
// Once chunks or headers have been flushed downstream (flusher.Flush() called),
// retries and failovers are strictly prohibited to prevent corrupted stream concatenations.
func (s *Stage) handleStreamingResponse(ctx context.Context, resp *http.Response, reqCtx *pipeline.RequestContext, cb *resilience.CircuitBreaker) error {
	defer resp.Body.Close()

	flusher, ok := reqCtx.Writer.(http.Flusher)
	if !ok {
		return model.NewGatewayError(
			http.StatusInternalServerError,
			model.ErrCodeInternalError,
			"Downstream response writer does not support flushing for SSE streaming.",
		)
	}

	// Set standard SSE headers
	reqCtx.Writer.Header().Set("Content-Type", "text/event-stream")
	reqCtx.Writer.Header().Set("Cache-Control", "no-cache")
	reqCtx.Writer.Header().Set("Connection", "keep-alive")
	reqCtx.Writer.Header().Set("X-Accel-Buffering", "no")
	reqCtx.Writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Past this point, headers have flushed to client. Never retry or failover.
	disconnected := make(chan struct{})
	defer close(disconnected)

	go func() {
		select {
		case <-reqCtx.Context.Done():
			resp.Body.Close()
		case <-disconnected:
		}
	}()

	var streamScanner *policy.StreamScanner
	if len(reqCtx.Policies) > 0 {
		streamScanner = policy.NewStreamScanner(reqCtx.Policies)
	}

	reader := bufio.NewReader(resp.Body)
	var totalTokensCounted int

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if streamScanner != nil {
				trimmed := bytes.TrimSpace(line)
				if bytes.HasPrefix(trimmed, []byte("data: ")) {
					payload := bytes.TrimPrefix(trimmed, []byte("data: "))
					if bytes.Equal(payload, []byte("[DONE]")) {
						// Stream completed
						tail, scanErr := streamScanner.Finish()
						if scanErr != nil {
							var violErr *policy.ErrStreamPolicyViolation
							ruleName := "POLICY_VIOLATION"
							if errors.As(scanErr, &violErr) {
								ruleName = violErr.Policy
							}
							resp.Body.Close()
							reqCtx.Writer.Write([]byte(fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"POLICY_VIOLATION\",\"message\":\"Stream terminated due to policy violation\",\"rule\":%q}}\n\n", ruleName)))
							flusher.Flush()
							reqCtx.ExecuteReconcile(0)
							reqCtx.ShortCircuited = true
							return model.NewGatewayError(http.StatusBadRequest, model.ErrCodePolicyViolation, scanErr.Error())
						}

						if len(tail) > 0 {
							chunkJSON, _ := json.Marshal(model.StreamChunk{
								Object: "chat.completion.chunk",
								Choices: []model.StreamChoice{
									{Index: 0, Delta: model.StreamChoiceDelta{Content: tail}},
								},
							})
							reqCtx.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", string(chunkJSON))))
							flusher.Flush()
						}

						reqCtx.Writer.Write([]byte("data: [DONE]\n\n"))
						flusher.Flush()
						reqCtx.ExecuteReconcile(totalTokensCounted)
						reqCtx.ShortCircuited = true
						return nil
					}

					var chunk model.StreamChunk
					if jsonErr := json.Unmarshal(payload, &chunk); jsonErr == nil {
						if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
							totalTokensCounted = chunk.Usage.TotalTokens
						}
						var deltaContent string
						if len(chunk.Choices) > 0 {
							deltaContent = chunk.Choices[0].Delta.Content
						}
						if deltaContent != "" {
							emitted, scanErr := streamScanner.Feed(deltaContent)
							if scanErr != nil {
								var violErr *policy.ErrStreamPolicyViolation
								ruleName := "POLICY_VIOLATION"
								if errors.As(scanErr, &violErr) {
									ruleName = violErr.Policy
								}
								resp.Body.Close()
								reqCtx.Writer.Write([]byte(fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"POLICY_VIOLATION\",\"message\":\"Stream terminated due to policy violation\",\"rule\":%q}}\n\n", ruleName)))
								flusher.Flush()
								reqCtx.ExecuteReconcile(0)
								reqCtx.ShortCircuited = true
								return model.NewGatewayError(http.StatusBadRequest, model.ErrCodePolicyViolation, scanErr.Error())
							}
							if len(emitted) > 0 {
								chunk.Choices[0].Delta.Content = emitted
								chunkBytes, _ := json.Marshal(chunk)
								reqCtx.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", string(chunkBytes))))
								flusher.Flush()
							}
						} else if len(chunk.Choices) > 0 && (chunk.Choices[0].Delta.Role != "" || chunk.Choices[0].FinishReason != nil) {
							chunkBytes, _ := json.Marshal(chunk)
							reqCtx.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", string(chunkBytes))))
							flusher.Flush()
						}
					} else {
						// Non-json data payload
						emitted, scanErr := streamScanner.Feed(string(payload))
						if scanErr != nil {
							resp.Body.Close()
							reqCtx.Writer.Write([]byte("event: error\ndata: {\"error\":{\"code\":\"POLICY_VIOLATION\"}}\n\n"))
							flusher.Flush()
							reqCtx.ExecuteReconcile(0)
							reqCtx.ShortCircuited = true
							return model.NewGatewayError(http.StatusBadRequest, model.ErrCodePolicyViolation, scanErr.Error())
						}
						if len(emitted) > 0 {
							reqCtx.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", emitted)))
							flusher.Flush()
						}
					}
				}
			} else {
				// No policies active: stream directly through
				trimmed := bytes.TrimSpace(line)
				if bytes.HasPrefix(trimmed, []byte("data: ")) {
					payload := bytes.TrimPrefix(trimmed, []byte("data: "))
					var chunk model.StreamChunk
					if jsonErr := json.Unmarshal(payload, &chunk); jsonErr == nil && chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
						totalTokensCounted = chunk.Usage.TotalTokens
					}
				}

				if _, writeErr := reqCtx.Writer.Write(line); writeErr != nil {
					return writeErr
				}
				if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
					flusher.Flush()
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if streamScanner != nil {
					tail, scanErr := streamScanner.Finish()
					if scanErr != nil {
						var violErr *policy.ErrStreamPolicyViolation
						ruleName := "POLICY_VIOLATION"
						if errors.As(scanErr, &violErr) {
							ruleName = violErr.Policy
						}
						resp.Body.Close()
						reqCtx.Writer.Write([]byte(fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"POLICY_VIOLATION\",\"message\":\"Stream terminated due to policy violation\",\"rule\":%q}}\n\n", ruleName)))
						flusher.Flush()
						reqCtx.ExecuteReconcile(0)
						reqCtx.ShortCircuited = true
						return model.NewGatewayError(http.StatusBadRequest, model.ErrCodePolicyViolation, scanErr.Error())
					}
					if len(tail) > 0 {
						chunkJSON, _ := json.Marshal(model.StreamChunk{
							Object: "chat.completion.chunk",
							Choices: []model.StreamChoice{
								{Index: 0, Delta: model.StreamChoiceDelta{Content: tail}},
							},
						})
						reqCtx.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", string(chunkJSON))))
						flusher.Flush()
					}
				}
				flusher.Flush()
				reqCtx.ExecuteReconcile(totalTokensCounted)
				reqCtx.ShortCircuited = true
				return nil
			}
			if reqCtx.Context.Err() != nil {
				return reqCtx.Context.Err()
			}
			if cb != nil {
				cb.RecordFailure()
			}
			// Emit structured SSE error frame per FAILURE-MODES.md
			reqCtx.Writer.Write([]byte("data: {\"error\":{\"message\":\"Upstream provider disconnected mid-stream\",\"type\":\"gateway_upstream_error\",\"code\":\"upstream_failure\"}}\n\ndata: [DONE]\n\n"))
			flusher.Flush()
			reqCtx.ExecuteReconcile(0)
			reqCtx.ShortCircuited = true
			return model.NewGatewayError(
				http.StatusBadGateway,
				model.ErrCodeUpstreamBadGateway,
				fmt.Sprintf("Stream interrupted from upstream: %v", err),
			)
		}
	}
}
