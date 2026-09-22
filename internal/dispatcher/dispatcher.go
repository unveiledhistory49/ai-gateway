package dispatcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

// bufferPool recycles 4KB byte slices for zero-copy streaming passes.
var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 4096)
		return &b
	},
}

// Stage implements Stage 3: Upstream Dispatcher.
type Stage struct {
	client *http.Client
}

// NewStage constructs a new Upstream Dispatcher stage with a tuned persistent HTTP client pool.
func NewStage(defaultTimeout time.Duration) *Stage {
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

	return &Stage{
		client: &http.Client{
			Transport: transport,
		},
	}
}

// NewStageWithClient allows injecting a custom HTTP client (e.g., for unit/integration testing).
func NewStageWithClient(client *http.Client) *Stage {
	return &Stage{
		client: client,
	}
}

// Name returns the identifier of this stage.
func (s *Stage) Name() string {
	return "upstream_dispatcher"
}

// Execute handles forwarding the request to the upstream provider and proxying the response.
func (s *Stage) Execute(reqCtx *pipeline.RequestContext) error {
	if reqCtx.Upstream == nil || reqCtx.Route == nil || reqCtx.ChatRequest == nil {
		return model.NewGatewayError(
			http.StatusInternalServerError,
			model.ErrCodeInternalError,
			"Cannot dispatch request: missing route or upstream context.",
		)
	}

	// Prepare upstream payload with mapped target model name
	upstreamReqPayload := *reqCtx.ChatRequest
	upstreamReqPayload.Model = reqCtx.TargetModel

	payloadBytes, err := json.Marshal(upstreamReqPayload)
	if err != nil {
		return model.NewGatewayError(
			http.StatusInternalServerError,
			model.ErrCodeInternalError,
			fmt.Sprintf("Failed to serialize upstream payload: %v", err),
		)
	}

	endpointURL := strings.TrimRight(reqCtx.Upstream.EndpointURL, "/") + "/v1/chat/completions"

	// Create cancellable context coupled to downstream client context
	upstreamCtx, cancel := context.WithCancel(reqCtx.Context)
	defer cancel()

	// If non-streaming, apply upstream timeout to context
	if !reqCtx.ChatRequest.Stream && reqCtx.Upstream.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		upstreamCtx, timeoutCancel = context.WithTimeout(upstreamCtx, reqCtx.Upstream.Timeout())
		defer timeoutCancel()
	}

	httpReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpointURL, bytes.NewReader(payloadBytes))
	if err != nil {
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

	// Vaulted upstream API key
	if reqCtx.Upstream.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+reqCtx.Upstream.APIKey)
	}

	// Dispatch request
	resp, err := s.client.Do(httpReq)
	if err != nil {
		if reqCtx.Context.Err() != nil {
			return reqCtx.Context.Err()
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return model.NewGatewayError(
				http.StatusGatewayTimeout,
				model.ErrCodeUpstreamTimeout,
				fmt.Sprintf("Upstream provider '%s' timed out: %v", reqCtx.Upstream.ID, err),
			)
		}
		return model.NewGatewayError(
			http.StatusBadGateway,
			model.ErrCodeUpstreamBadGateway,
			fmt.Sprintf("Failed to reach upstream provider '%s': %v", reqCtx.Upstream.ID, err),
		)
	}
	defer resp.Body.Close()

	// Handle upstream non-2xx status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.handleUpstreamError(resp, reqCtx.Upstream.ID)
	}

	// Dispatch based on stream flag
	if reqCtx.ChatRequest.Stream {
		return s.handleStreamingResponse(upstreamCtx, resp, reqCtx)
	}

	return s.handleSyncResponse(resp, reqCtx)
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

	reqCtx.Writer.Header().Set("Content-Type", "application/json")
	reqCtx.Writer.WriteHeader(resp.StatusCode)
	reqCtx.Writer.Write(bodyBytes)

	reqCtx.ShortCircuited = true
	return nil
}

// handleStreamingResponse streams SSE chunks incrementally with active upstream socket teardown.
func (s *Stage) handleStreamingResponse(ctx context.Context, resp *http.Response, reqCtx *pipeline.RequestContext) error {
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

	// Monitor client disconnection in background goroutine to hard-close upstream body immediately
	disconnected := make(chan struct{})
	defer close(disconnected)

	go func() {
		select {
		case <-reqCtx.Context.Done():
			// Client dropped connection. Hard close the upstream response body
			// to force immediate TCP RST/FIN to upstream provider.
			resp.Body.Close()
		case <-disconnected:
			// Stream completed normally
		}
	}()

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := reqCtx.Writer.Write(line); writeErr != nil {
				return writeErr
			}
			// Flush on empty line delimiter between SSE frames
			if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
				flusher.Flush()
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				flusher.Flush()
				reqCtx.ShortCircuited = true
				return nil
			}
			if reqCtx.Context.Err() != nil {
				// Client disconnected
				return reqCtx.Context.Err()
			}
			return model.NewGatewayError(
				http.StatusBadGateway,
				model.ErrCodeUpstreamBadGateway,
				fmt.Sprintf("Stream interrupted from upstream: %v", err),
			)
		}
	}
}
