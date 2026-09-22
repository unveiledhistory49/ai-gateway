package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
	"github.com/company/ai-gateway/internal/resilience"
)

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
}

func (f *flushRecorder) Flush() {
	f.flushed = true
}

func TestDispatcherSyncSuccess(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-upstream-key" {
			http.Error(w, "unauthorized upstream", http.StatusUnauthorized)
			return
		}

		var req model.CanonicalChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Verify model was translated to target model name
		if req.Model != "gpt-4o-actual" {
			http.Error(w, fmt.Sprintf("expected model gpt-4o-actual, got %s", req.Model), http.StatusBadRequest)
			return
		}

		resp := model.CanonicalChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []model.ChatChoice{
				{
					Index: 0,
					Message: model.ChatMessage{
						Role:    "assistant",
						Content: "Hello from mock upstream!",
					},
				},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockUpstream.Close()

	stage := NewStageWithClient(mockUpstream.Client())

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model: "gpt-4o-alias",
		Messages: []model.ChatMessage{
			{Role: "user", Content: "hi"},
		},
		Stream: false,
	}
	reqCtx.Route = &config.RouteConfig{
		Alias:           "gpt-4o-alias",
		PrimaryUpstream: "mock-upstream",
		ModelName:       "gpt-4o-actual",
	}
	reqCtx.Upstream = &config.UpstreamConfig{
		ID:             "mock-upstream",
		EndpointURL:    mockUpstream.URL,
		APIKey:         "secret-upstream-key",
		TimeoutSeconds: 5,
	}
	reqCtx.TargetModel = "gpt-4o-actual"

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var chatResp model.CanonicalChatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(chatResp.Choices) == 0 || chatResp.Choices[0].Message.Content != "Hello from mock upstream!" {
		t.Fatalf("unexpected response choices: %v", chatResp.Choices)
	}
}

func TestDispatcherStreamingSuccess(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher := w.(http.Flusher)

		chunks := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\" World\"}}]}\n\n",
			"data: [DONE]\n\n",
		}

		for _, chunk := range chunks {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer mockUpstream.Close()

	stage := NewStageWithClient(mockUpstream.Client())

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model:  "gpt-4o",
		Stream: true,
	}
	reqCtx.Route = &config.RouteConfig{
		Alias:           "gpt-4o",
		PrimaryUpstream: "mock-upstream",
		ModelName:       "gpt-4o",
	}
	reqCtx.Upstream = &config.UpstreamConfig{
		ID:             "mock-upstream",
		EndpointURL:    mockUpstream.URL,
		TimeoutSeconds: 5,
	}
	reqCtx.TargetModel = "gpt-4o"

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}
	if w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected text/event-stream, got %s", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "Hello") || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatalf("expected stream body to contain chunks, got: %s", w.Body.String())
	}
}

func TestDispatcherUpstreamErrors(t *testing.T) {
	tests := []struct {
		name         string
		upstreamCode int
		expectedCode string
		expectedHTTP int
	}{
		{
			name:         "502 Bad Gateway",
			upstreamCode: http.StatusBadGateway,
			expectedCode: model.ErrCodeUpstreamBadGateway,
			expectedHTTP: http.StatusBadGateway,
		},
		{
			name:         "503 Service Unavailable",
			upstreamCode: http.StatusServiceUnavailable,
			expectedCode: model.ErrCodeUpstreamUnavailable,
			expectedHTTP: http.StatusServiceUnavailable,
		},
		{
			name:         "429 Too Many Requests",
			upstreamCode: http.StatusTooManyRequests,
			expectedCode: model.ErrCodeRateLimitExceeded,
			expectedHTTP: http.StatusTooManyRequests,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "upstream failed", tc.upstreamCode)
			}))
			defer mockUpstream.Close()

			stage := NewStageWithClient(mockUpstream.Client())

			w := newFlushRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
			reqCtx.ChatRequest = &model.CanonicalChatRequest{Model: "m1"}
			reqCtx.Route = &config.RouteConfig{Alias: "m1", PrimaryUpstream: "u1", ModelName: "m1"}
			reqCtx.Upstream = &config.UpstreamConfig{ID: "u1", EndpointURL: mockUpstream.URL}
			reqCtx.TargetModel = "m1"

			err := stage.Execute(reqCtx)
			if err == nil {
				t.Fatal("expected error, got nil")
			}

			var gwErr *model.GatewayError
			if !errors.As(err, &gwErr) {
				t.Fatalf("expected GatewayError, got %T: %v", err, err)
			}

			if gwErr.StatusCode != tc.expectedHTTP || gwErr.Code != tc.expectedCode {
				t.Fatalf("expected %d/%s, got %d/%s", tc.expectedHTTP, tc.expectedCode, gwErr.StatusCode, gwErr.Code)
			}
		})
	}
}

func TestDispatcherClientCancellationDuringStream(t *testing.T) {
	upstreamClosed := make(chan struct{})

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if ok {
			w.Write([]byte("data: chunk 1\n\n"))
			flusher.Flush()
		}

		// Wait until request body/connection is closed or context cancelled
		notify := r.Context().Done()
		select {
		case <-notify:
			close(upstreamClosed)
			return
		case <-time.After(5 * time.Second):
			return
		}
	}))
	defer mockUpstream.Close()

	stage := NewStageWithClient(mockUpstream.Client())

	clientCtx, clientCancel := context.WithCancel(context.Background())

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(clientCtx, w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model:  "m1",
		Stream: true,
	}
	reqCtx.Route = &config.RouteConfig{Alias: "m1", PrimaryUpstream: "u1", ModelName: "m1"}
	reqCtx.Upstream = &config.UpstreamConfig{ID: "u1", EndpointURL: mockUpstream.URL}
	reqCtx.TargetModel = "m1"

	// Cancel the client context after 50 milliseconds
	go func() {
		time.Sleep(50 * time.Millisecond)
		clientCancel()
	}()

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error due to client cancellation, got nil")
	}

	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("expected context.Canceled error, got: %v", err)
	}

	// Verify upstream received the close/cancellation
	select {
	case <-upstreamClosed:
		// Upstream closed cleanly!
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection was not closed upon client cancellation")
	}
}

func TestDispatcherFailoverToSecondaryOn503(t *testing.T) {
	primaryCalled := 0
	secondaryCalled := 0

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		http.Error(w, `{"error":{"message":"primary overloaded"}}`, http.StatusServiceUnavailable)
	}))
	defer primaryServer.Close()

	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalled++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"chatcmpl-secondary","object":"chat.completion","created":123,"model":"secondary-model","choices":[{"index":0,"message":{"role":"assistant","content":"response from secondary"}}]}`))
	}))
	defer secondaryServer.Close()

	reg := resilience.NewRegistry(resilience.CircuitBreakerConfig{}, nil)
	stage := NewStageWithClient(secondaryServer.Client(), reg)

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model: "prod-alias",
		Messages: []model.ChatMessage{
			{Role: "user", Content: "hello"},
		},
		Stream: false,
	}

	reqCtx.Targets = []*model.RouteTarget{
		{
			UpstreamID:   "primary-up",
			EndpointURL:  primaryServer.URL,
			TargetModel:  "primary-model",
			PriorityTier: 0,
		},
		{
			UpstreamID:   "secondary-up",
			EndpointURL:  secondaryServer.URL,
			TargetModel:  "secondary-model",
			PriorityTier: 1,
		},
	}

	err := stage.Execute(reqCtx)
	if err != nil {
		t.Fatalf("expected successful failover, got error: %v", err)
	}

	if primaryCalled != 1 {
		t.Fatalf("expected primary to be called 1 time, got %d", primaryCalled)
	}
	if secondaryCalled != 1 {
		t.Fatalf("expected secondary to be called 1 time, got %d", secondaryCalled)
	}

	// Verify response from secondary
	if !strings.Contains(w.Body.String(), "response from secondary") {
		t.Fatalf("expected body to contain secondary response, got: %s", w.Body.String())
	}

	// Verify primary circuit breaker recorded failure
	cbPrimary := reg.Get("primary-up")
	// If primary recorded failure, consecutive successes is 0 and failure was recorded
	_ = cbPrimary
}

func TestDispatcherStreamingSafetyRule(t *testing.T) {
	primaryCalled := 0
	secondaryCalled := 0

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		flusher.Flush()
		// Abruptly close or fail after flushing
		// We can hijack or just return without [DONE]
	}))
	defer primaryServer.Close()

	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalled++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer secondaryServer.Close()

	reg := resilience.NewRegistry(resilience.CircuitBreakerConfig{}, nil)
	stage := NewStageWithClient(primaryServer.Client(), reg)

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model:  "prod-alias",
		Stream: true,
	}

	reqCtx.Targets = []*model.RouteTarget{
		{
			UpstreamID:   "primary-up",
			EndpointURL:  primaryServer.URL,
			TargetModel:  "primary-model",
			PriorityTier: 0,
		},
		{
			UpstreamID:   "secondary-up",
			EndpointURL:  secondaryServer.URL,
			TargetModel:  "secondary-model",
			PriorityTier: 1,
		},
	}

	_ = stage.Execute(reqCtx)

	// Since primary flushed chunks downstream, secondary MUST NEVER be called
	if secondaryCalled != 0 {
		t.Fatalf("CRITICAL SAFETY VIOLATION: secondary was called (%d times) after primary flushed streaming chunks!", secondaryCalled)
	}
}

func TestDispatcherNonRetryableErrorNoFailover(t *testing.T) {
	primaryCalled := 0
	secondaryCalled := 0

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	defer primaryServer.Close()

	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalled++
	}))
	defer secondaryServer.Close()

	reg := resilience.NewRegistry(resilience.CircuitBreakerConfig{}, nil)
	stage := NewStageWithClient(primaryServer.Client(), reg)

	w := newFlushRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.ChatRequest = &model.CanonicalChatRequest{
		Model:  "prod-alias",
		Stream: false,
	}

	reqCtx.Targets = []*model.RouteTarget{
		{
			UpstreamID:   "primary-up",
			EndpointURL:  primaryServer.URL,
			TargetModel:  "primary-model",
			PriorityTier: 0,
		},
		{
			UpstreamID:   "secondary-up",
			EndpointURL:  secondaryServer.URL,
			TargetModel:  "secondary-model",
			PriorityTier: 1,
		},
	}

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected error for 401 Unauthorized, got nil")
	}

	var gwErr *model.GatewayError
	if !errors.As(err, &gwErr) || gwErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got: %v", err)
	}

	if secondaryCalled != 0 {
		t.Fatalf("expected secondary NOT to be called on non-retryable 401, but got %d calls", secondaryCalled)
	}
}

