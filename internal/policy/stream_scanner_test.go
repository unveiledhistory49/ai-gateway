package policy

import (
	"strings"
	"testing"

	"github.com/company/ai-gateway/internal/config"
)

func TestStreamScannerCrossChunkDetectionBlock(t *testing.T) {
	policy := []*config.PolicyConfig{
		{
			ID:       "pci-block",
			Name:     "pci-block",
			Action:   "BLOCK",
			Patterns: []string{"credit_card"},
		},
	}

	// Valid card: "4242-4242-4242-4242" (19 bytes)
	// We split it across 4 separate chunk tokens:
	cardChunks := []string{
		"4242-",
		"4242-",
		"4242-",
		"4242",
	}

	scanner := NewStreamScanner(policy)

	// Feed initial clean prefix that fills buffer close to lookaheadCap (128)
	cleanPrefix := "The payment account details for the transaction are registered as follows: card number: "
	emitted, err := scanner.Feed(cleanPrefix)
	if err != nil {
		t.Fatalf("unexpected error feeding prefix: %v", err)
	}

	var allEmitted strings.Builder
	allEmitted.WriteString(emitted)

	// Now feed the 4 card chunks one by one
	var violationTriggered bool
	for _, chunk := range cardChunks {
		chunkEmitted, feedErr := scanner.Feed(chunk)
		if feedErr != nil {
			violationTriggered = true
			if !strings.Contains(feedErr.Error(), "policy violation") {
				t.Fatalf("expected policy violation error, got: %v", feedErr)
			}
			break
		}
		allEmitted.WriteString(chunkEmitted)
	}

	// If not triggered during Feed, call Finish()
	if !violationTriggered {
		tail, finishErr := scanner.Finish()
		if finishErr != nil {
			violationTriggered = true
		} else {
			allEmitted.WriteString(tail)
		}
	}

	if !violationTriggered {
		t.Fatal("expected cross-chunk credit card to trigger policy violation, but it passed clean")
	}

	// CRITICAL SECURITY ASSERTION:
	// The emitted text must NEVER contain any portion of the credit card!
	emittedStr := allEmitted.String()
	if strings.Contains(emittedStr, "4242") {
		t.Fatalf("CRITICAL SECURITY VIOLATION: emitted text leaked credit card prefix: %s", emittedStr)
	}
}

func TestStreamScannerCrossChunkDetectionMask(t *testing.T) {
	policy := []*config.PolicyConfig{
		{
			ID:       "ssn-mask",
			Name:     "ssn-mask",
			Action:   "MASK",
			Patterns: []string{"ssn"},
		},
	}

	// Valid SSN: "123-45-6789" (11 bytes)
	// Split across 3 tokens: "123-", "45-", "6789"
	ssnChunks := []string{
		"123-",
		"45-",
		"6789",
	}

	scanner := NewStreamScanner(policy)

	var emitted strings.Builder

	// Feed padding
	p, err := scanner.Feed("Customer SSN is ")
	if err != nil {
		t.Fatalf("unexpected feed error: %v", err)
	}
	emitted.WriteString(p)

	// Feed split tokens
	for _, chunk := range ssnChunks {
		part, err := scanner.Feed(chunk)
		if err != nil {
			t.Fatalf("unexpected error feeding chunk %s: %v", chunk, err)
		}
		emitted.WriteString(part)
	}

	p2, err := scanner.Feed(" verified.")
	if err != nil {
		t.Fatalf("unexpected feed error: %v", err)
	}
	emitted.WriteString(p2)

	// Finish stream
	tail, err := scanner.Finish()
	if err != nil {
		t.Fatalf("unexpected finish error: %v", err)
	}
	emitted.WriteString(tail)

	finalOutput := emitted.String()
	if !strings.Contains(finalOutput, "[REDACTED:ssn-mask]") {
		t.Fatalf("expected output to contain redacted token [REDACTED:ssn-mask], got: %s", finalOutput)
	}
	if strings.Contains(finalOutput, "123-45-6789") {
		t.Fatalf("output still contains unredacted SSN: %s", finalOutput)
	}
}

func TestStreamScannerCleanStream(t *testing.T) {
	policy := []*config.PolicyConfig{
		{
			ID:       "pci-block",
			Name:     "pci-block",
			Action:   "BLOCK",
			Patterns: []string{"credit_card"},
		},
	}

	scanner := NewStreamScanner(policy)

	tokens := []string{
		"Hello",
		" this",
		" is",
		" a",
		" completely",
		" normal",
		" AI",
		" response",
		" with",
		" no",
		" secrets.",
	}

	var out strings.Builder
	for _, tok := range tokens {
		s, err := scanner.Feed(tok)
		if err != nil {
			t.Fatalf("unexpected feed error: %v", err)
		}
		out.WriteString(s)
	}

	tail, err := scanner.Finish()
	if err != nil {
		t.Fatalf("unexpected finish error: %v", err)
	}
	out.WriteString(tail)

	expected := "Hello this is a completely normal AI response with no secrets."
	if out.String() != expected {
		t.Fatalf("expected '%s', got '%s'", expected, out.String())
	}
}
