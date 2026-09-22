package policy

import (
	"fmt"

	"github.com/company/ai-gateway/internal/config"
)

// Default lookahead parameters per ADR-004.
const (
	DefaultLookaheadCap = 128
	DefaultOverlapMargin = 64
)

// ErrStreamPolicyViolation indicates a BLOCK policy was triggered during streaming.
type ErrStreamPolicyViolation struct {
	Policy string
	Rule   string
}

func (e *ErrStreamPolicyViolation) Error() string {
	return fmt.Sprintf("stream terminated due to policy violation: %s (rule: %s)", e.Policy, e.Rule)
}

// StreamScanner implements the fixed sliding-window lookahead buffer for SSE streams.
type StreamScanner struct {
	lookaheadCap int
	overlap      int
	policies     []*config.PolicyConfig
	engine       *Engine
	buf          []byte
	violated     bool
	violationErr error
}

// NewStreamScanner constructs a new StreamScanner with the given policies.
func NewStreamScanner(policies []*config.PolicyConfig, engine ...*Engine) *StreamScanner {
	var eng *Engine
	if len(engine) > 0 && engine[0] != nil {
		eng = engine[0]
	} else {
		eng = NewEngine()
	}

	return &StreamScanner{
		lookaheadCap: DefaultLookaheadCap,
		overlap:      DefaultOverlapMargin,
		policies:     policies,
		engine:       eng,
		buf:          make([]byte, 0, DefaultLookaheadCap*2),
	}
}

// NewStreamScannerWithParams constructs a StreamScanner with custom window and overlap sizes.
func NewStreamScannerWithParams(lookaheadCap, overlap int, policies []*config.PolicyConfig, engine *Engine) *StreamScanner {
	if lookaheadCap <= 0 {
		lookaheadCap = DefaultLookaheadCap
	}
	if overlap <= 0 {
		overlap = DefaultOverlapMargin
	}
	if engine == nil {
		engine = NewEngine()
	}

	return &StreamScanner{
		lookaheadCap: lookaheadCap,
		overlap:      overlap,
		policies:     policies,
		engine:       engine,
		buf:          make([]byte, 0, lookaheadCap*2),
	}
}

// Feed ingests an incremental token text fragment into the lookahead buffer.
// It returns any text that is eligible to be safely flushed to downstream.
func (s *StreamScanner) Feed(token string) (string, error) {
	if s.violated {
		return "", s.violationErr
	}
	if len(token) == 0 {
		return "", nil
	}

	s.buf = append(s.buf, []byte(token)...)

	// If buffer hasn't reached the lookahead threshold, hold back
	if len(s.buf) < s.lookaheadCap {
		return "", nil
	}

	// Scan current buffer
	res, err := s.engine.Scan(string(s.buf), s.policies)
	if err != nil {
		return "", fmt.Errorf("DLP scan error: %w", err)
	}

	if res.Violated {
		if res.Action == "BLOCK" {
			s.violated = true
			s.violationErr = &ErrStreamPolicyViolation{
				Policy: res.ViolatedPolicy,
				Rule:   res.ViolatedRule,
			}
			// Zero out buffer per ADR-004 to eliminate leakage
			s.buf = nil
			return "", s.violationErr
		}

		// Action == "MASK": update buffer with redacted text
		s.buf = []byte(res.RedactedText)
	}

	// Release bytes exceeding the overlap margin
	releaseLen := len(s.buf) - s.overlap
	if releaseLen > 0 {
		toEmit := string(s.buf[:releaseLen])
		s.buf = append([]byte(nil), s.buf[releaseLen:]...)
		return toEmit, nil
	}

	return "", nil
}

// Finish evaluates all remaining bytes in the buffer and returns the tail.
func (s *StreamScanner) Finish() (string, error) {
	if s.violated {
		return "", s.violationErr
	}
	if len(s.buf) == 0 {
		return "", nil
	}

	res, err := s.engine.Scan(string(s.buf), s.policies)
	if err != nil {
		return "", fmt.Errorf("DLP scan error: %w", err)
	}

	if res.Violated {
		if res.Action == "BLOCK" {
			s.violated = true
			s.violationErr = &ErrStreamPolicyViolation{
				Policy: res.ViolatedPolicy,
				Rule:   res.ViolatedRule,
			}
			s.buf = nil
			return "", s.violationErr
		}

		s.buf = []byte(res.RedactedText)
	}

	toEmit := string(s.buf)
	s.buf = nil
	return toEmit, nil
}
