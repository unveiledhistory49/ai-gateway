package tracing

import (
	"net/http"
	"strings"
	"testing"
)

func TestNewTraceID(t *testing.T) {
	id := NewTraceID()
	if len(id) != 32 {
		t.Fatalf("expected 32 hex chars, got %d (%s)", len(id), id)
	}
	if !isHex(id) {
		t.Fatalf("expected hex string, got %s", id)
	}
	if id == AllZeroTraceID {
		t.Fatal("trace ID cannot be all zeros")
	}

	// Verify randomness
	id2 := NewTraceID()
	if id == id2 {
		t.Fatal("consecutive trace IDs should not be identical")
	}
}

func TestNewSpanID(t *testing.T) {
	id := NewSpanID()
	if len(id) != 16 {
		t.Fatalf("expected 16 hex chars, got %d (%s)", len(id), id)
	}
	if !isHex(id) {
		t.Fatalf("expected hex string, got %s", id)
	}
	if id == AllZeroSpanID {
		t.Fatal("span ID cannot be all zeros")
	}

	id2 := NewSpanID()
	if id == id2 {
		t.Fatal("consecutive span IDs should not be identical")
	}
}

func TestNewRequestID(t *testing.T) {
	id := NewRequestID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("expected 5 UUID segments, got %d (%s)", len(parts), id)
	}
	if len(id) != 36 {
		t.Fatalf("expected UUID length 36, got %d (%s)", len(id), id)
	}

	id2 := NewRequestID()
	if id == id2 {
		t.Fatal("consecutive request IDs should not be identical")
	}
}

func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		valid      bool
		wantTrace  string
		wantSpan   string
	}{
		{
			name:      "valid standard header",
			header:    "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			valid:     true,
			wantTrace: "4bf92f3577b34da6a3ce929d0e0e4736",
			wantSpan:  "00f067aa0ba902b7",
		},
		{
			name:   "empty header",
			header: "",
			valid:  false,
		},
		{
			name:   "wrong length",
			header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
			valid:  false,
		},
		{
			name:   "invalid version",
			header: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			valid:  false,
		},
		{
			name:   "all zero trace id",
			header: "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
			valid:  false,
		},
		{
			name:   "all zero span id",
			header: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
			valid:  false,
		},
		{
			name:   "non-hex characters in trace id",
			header: "00-4bf92f3577b34da6a3ce929d0e0e47zz-00f067aa0ba902b7-01",
			valid:  false,
		},
		{
			name:   "invalid flags",
			header: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",
			valid:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			traceID, spanID, ok := ParseTraceparent(tt.header)
			if ok != tt.valid {
				t.Fatalf("expected valid=%v, got %v", tt.valid, ok)
			}
			if tt.valid {
				if traceID != tt.wantTrace {
					t.Errorf("expected traceID=%s, got %s", tt.wantTrace, traceID)
				}
				if spanID != tt.wantSpan {
					t.Errorf("expected spanID=%s, got %s", tt.wantSpan, spanID)
				}
			}
		})
	}
}

func TestExtractOrGenerate(t *testing.T) {
	// Valid incoming
	validHeader := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tID, sID := ExtractOrGenerate(validHeader)
	if tID != "4bf92f3577b34da6a3ce929d0e0e4736" || sID != "00f067aa0ba902b7" {
		t.Fatalf("failed to preserve valid incoming traceparent: %s, %s", tID, sID)
	}

	// Missing incoming
	tID2, sID2 := ExtractOrGenerate("")
	if len(tID2) != 32 || len(sID2) != 16 {
		t.Fatalf("failed to generate valid IDs on empty header: %s, %s", tID2, sID2)
	}

	// Corrupt incoming
	tID3, sID3 := ExtractOrGenerate("garbage")
	if len(tID3) != 32 || len(sID3) != 16 {
		t.Fatalf("failed to generate valid IDs on corrupt header: %s, %s", tID3, sID3)
	}
}

func TestInjectHTTPHeaders(t *testing.T) {
	headers := make(http.Header)
	traceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	spanID := "00f067aa0ba902b7"
	reqID := "550e8400-e29b-41d4-a716-446655440000"

	InjectHTTPHeaders(headers, traceID, spanID, reqID)

	expectedTP := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if headers.Get(TraceparentHeader) != expectedTP {
		t.Errorf("expected traceparent %s, got %s", expectedTP, headers.Get(TraceparentHeader))
	}
	if headers.Get(RequestIDHeader) != reqID {
		t.Errorf("expected X-Request-ID %s, got %s", reqID, headers.Get(RequestIDHeader))
	}
}
