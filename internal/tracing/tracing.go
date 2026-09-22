package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

const (
	TraceparentHeader = "traceparent"
	RequestIDHeader    = "X-Request-ID"
	DefaultTraceFlags = "01"
	AllZeroTraceID    = "00000000000000000000000000000000"
	AllZeroSpanID     = "0000000000000000"
)

// SpanContext holds the trace and span correlation identifiers.
type SpanContext struct {
	TraceID string
	SpanID  string
	Sampled bool
}

// NewTraceID generates a cryptographically secure 128-bit (16-byte) random trace ID (32 hex characters).
func NewTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	// In the astronomically unlikely event of all zeros, set least significant byte
	if b == [16]byte{} {
		b[15] = 1
	}
	return hex.EncodeToString(b[:])
}

// NewSpanID generates a cryptographically secure 64-bit (8-byte) random span ID (16 hex characters).
func NewSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if b == [8]byte{} {
		b[7] = 1
	}
	return hex.EncodeToString(b[:])
}

// NewRequestID generates a standard UUIDv4 formatted string.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// FormatTraceparent formats the trace ID and span ID into W3C traceparent representation.
func FormatTraceparent(traceID, spanID string) string {
	return fmt.Sprintf("00-%s-%s-%s", traceID, spanID, DefaultTraceFlags)
}

// ParseTraceparent parses and validates a W3C traceparent header.
// Format: 00-<32-hex-trace-id>-<16-hex-span-id>-<2-hex-flags>
// Returns traceID, spanID, and true if valid.
func ParseTraceparent(header string) (string, string, bool) {
	header = strings.TrimSpace(header)
	if len(header) != 55 {
		return "", "", false
	}

	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		return "", "", false
	}

	version := parts[0]
	traceID := parts[1]
	spanID := parts[2]
	flags := parts[3]

	// Version must be "00" (or forward compatible non-"ff", but version 00 has length 55)
	if version != "00" {
		return "", "", false
	}

	// Trace ID: 32 hex chars, not all zero
	if len(traceID) != 32 || traceID == AllZeroTraceID || !isHex(traceID) {
		return "", "", false
	}

	// Span ID: 16 hex chars, not all zero
	if len(spanID) != 16 || spanID == AllZeroSpanID || !isHex(spanID) {
		return "", "", false
	}

	// Flags: 2 hex chars
	if len(flags) != 2 || !isHex(flags) {
		return "", "", false
	}

	return traceID, spanID, true
}

// ExtractOrGenerate extracts W3C traceparent from header or generates new trace ID and span ID.
func ExtractOrGenerate(header string) (traceID string, spanID string) {
	if tid, sid, ok := ParseTraceparent(header); ok {
		return tid, sid
	}
	return NewTraceID(), NewSpanID()
}

// InjectHTTPHeaders injects traceparent and X-Request-ID headers into the given http.Header.
func InjectHTTPHeaders(headers http.Header, traceID, spanID, requestID string) {
	if traceID != "" && spanID != "" {
		headers.Set(TraceparentHeader, FormatTraceparent(traceID, spanID))
	}
	if requestID != "" {
		headers.Set(RequestIDHeader, requestID)
	}
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
