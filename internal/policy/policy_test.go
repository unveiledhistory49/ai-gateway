package policy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/company/ai-gateway/internal/config"
	"github.com/company/ai-gateway/internal/model"
	"github.com/company/ai-gateway/internal/pipeline"
)

func TestCreditCardLuhnValidation(t *testing.T) {
	// Valid Luhn numbers
	validCards := []string{
		"4242424242424242",
		"4242-4242-4242-4242",
		"4242 4242 4242 4242",
		"4111111111111111",
		"4012888888881881",
		"5105105105105100",
		"378282246310005",
	}

	for _, card := range validCards {
		if !ValidateLuhn(card) {
			t.Errorf("expected card %s to pass Luhn check", card)
		}
	}

	// Invalid Luhn numbers
	invalidCards := []string{
		"4111222233334444",      // Checksum invalid
		"4111-2222-3333-4444", // Checksum invalid
		"1234567890123456",      // Checksum invalid
		"1234",                  // Too short
		"abcdefghijklmnop",      // Non-digits
	}

	for _, card := range invalidCards {
		if ValidateLuhn(card) {
			t.Errorf("expected card %s to fail Luhn check", card)
		}
	}
}

func TestSSNValidation(t *testing.T) {
	validSSNs := []string{
		"123-45-6789",
		"001-01-0001",
		"899-99-9999",
	}

	for _, ssn := range validSSNs {
		spans := FindSSNSpans("User SSN is " + ssn + " recorded.")
		if len(spans) != 1 {
			t.Errorf("expected valid SSN %s to be detected, got %d spans", ssn, len(spans))
		}
	}

	invalidSSNs := []string{
		"000-45-6789", // Area 000 invalid
		"666-45-6789", // Area 666 invalid
		"900-45-6789", // Area >= 900 invalid
		"950-45-6789", // Area >= 900 invalid
		"123-00-6789", // Group 00 invalid
		"123-45-0000", // Serial 0000 invalid
	}

	for _, ssn := range invalidSSNs {
		spans := FindSSNSpans("Invalid SSN " + ssn + " here.")
		if len(spans) != 0 {
			t.Errorf("expected invalid SSN %s to be ignored, but was detected", ssn)
		}
	}
}

func TestAWSKeyDetection(t *testing.T) {
	eng := NewEngine()
	policy := []*config.PolicyConfig{
		{
			ID:       "aws-guard",
			Name:     "aws-guard",
			Action:   "BLOCK",
			Patterns: []string{"aws_key"},
		},
	}

	// Secret access key pattern: (?i)aws(.{0,20})?['\"][0-9a-zA-Z/+]{40}['\"]
	textSecret := `export AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`
	res, err := eng.Scan(textSecret, policy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if !res.Violated || res.Action != "BLOCK" {
		t.Fatalf("expected AWS secret key to trigger BLOCK, got: %+v", res)
	}

	// Access key ID pattern: AKIA...
	textID := `My key ID is AKIAIOSFODNN7EXAMPLE.`
	resID, err := eng.Scan(textID, policy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if !resID.Violated || resID.Action != "BLOCK" {
		t.Fatalf("expected AWS access key ID to trigger BLOCK, got: %+v", resID)
	}
}

func TestPrivateKeyHeaderDetection(t *testing.T) {
	eng := NewEngine()
	policy := []*config.PolicyConfig{
		{
			ID:       "privkey-guard",
			Name:     "privkey-guard",
			Action:   "BLOCK",
			Patterns: []string{"private_key"},
		},
	}

	samples := []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"-----BEGIN EC PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	}

	for _, s := range samples {
		res, err := eng.Scan(s, policy)
		if err != nil {
			t.Fatalf("scan error: %v", err)
		}
		if !res.Violated || res.Action != "BLOCK" {
			t.Fatalf("expected private key header %s to be blocked, got: %+v", s, res)
		}
	}
}

func TestShannonEntropyDetection(t *testing.T) {
	// Normal English text should have low entropy
	englishText := "pneumonoultramicroscopicsilicovolcanoconiosis"
	entEnglish := ShannonEntropy(englishText)
	if entEnglish >= 4.5 {
		t.Fatalf("expected English word to have entropy < 4.5, got %f", entEnglish)
	}

	// High entropy base64 random secret
	randomSecret := "dGhpcyBpcyBhIHJhbmRvbSBzZWNyZXQga2V5IDEyMzQ1Ng=="
	entSecret := ShannonEntropy(randomSecret)
	if entSecret < 4.5 {
		t.Fatalf("expected random secret to have entropy >= 4.5, got %f", entSecret)
	}

	eng := NewEngine()
	policy := []*config.PolicyConfig{
		{
			ID:       "entropy-policy",
			Name:     "entropy-policy",
			Action:   "BLOCK",
			Patterns: []string{"high_entropy"},
		},
	}

	res, err := eng.Scan("Here is the secret: "+randomSecret, policy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if !res.Violated || res.Action != "BLOCK" {
		t.Fatalf("expected high-entropy token to be blocked, got: %+v", res)
	}

	// Clean text without high-entropy token
	resClean, err := eng.Scan("Here is a regular sentence with nothing secret.", policy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if resClean.Violated {
		t.Fatalf("expected clean text not to trigger violation, got: %+v", resClean)
	}
}

func TestPolicyActionMaskVsBlock(t *testing.T) {
	eng := NewEngine()

	// 1. BLOCK Action: rejects with violation
	blockPolicy := []*config.PolicyConfig{
		{
			ID:       "pci-block",
			Name:     "pci-block",
			Action:   "BLOCK",
			Patterns: []string{"credit_card"},
		},
	}

	text := "Card is 4242-4242-4242-4242 for payment."
	resBlock, err := eng.Scan(text, blockPolicy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if !resBlock.Violated || resBlock.Action != "BLOCK" || resBlock.ViolatedPolicy != "pci-block" {
		t.Fatalf("expected BLOCK action, got: %+v", resBlock)
	}

	// 2. MASK / REDACT Action: replaces secrets with [REDACTED:<policy_name>]
	maskPolicy := []*config.PolicyConfig{
		{
			ID:       "pci-mask",
			Name:     "pci-mask",
			Action:   "MASK",
			Patterns: []string{"credit_card"},
		},
	}

	resMask, err := eng.Scan(text, maskPolicy)
	if err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if !resMask.Violated || resMask.Action != "MASK" {
		t.Fatalf("expected MASK action, got: %+v", resMask)
	}
	expectedRedacted := "Card is [REDACTED:pci-mask] for payment."
	if resMask.RedactedText != expectedRedacted {
		t.Fatalf("expected '%s', got '%s'", expectedRedacted, resMask.RedactedText)
	}
}

func TestPrePolicyStage(t *testing.T) {
	eng := NewEngine()
	stage := NewPrePolicyStage(eng)

	policies := []*config.PolicyConfig{
		{
			ID:       "pci-guard",
			Name:     "pci-guard",
			Action:   "BLOCK",
			Patterns: []string{"credit_card"},
		},
		{
			ID:       "ssn-redact",
			Name:     "ssn-redact",
			Action:   "MASK",
			Patterns: []string{"ssn"},
		},
	}

	// 1. Test prompt with credit card is blocked with HTTP 400 POLICY_VIOLATION
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx1 := pipeline.NewRequestContext(context.Background(), w1, r1)
	reqCtx1.ChatRequest = &model.CanonicalChatRequest{
		Messages: []model.ChatMessage{
			{Role: "user", Content: "Charge my card 4242-4242-4242-4242"},
		},
	}
	reqCtx1.Policies = policies

	err1 := stage.Execute(reqCtx1)
	if err1 == nil {
		t.Fatal("expected error on blocked card, got nil")
	}
	var gwErr *model.GatewayError
	if !errors.As(err1, &gwErr) || gwErr.StatusCode != http.StatusBadRequest || gwErr.Code != model.ErrCodePolicyViolation {
		t.Fatalf("expected 400 POLICY_VIOLATION error, got: %v", err1)
	}
	if !strings.Contains(gwErr.Message, "pci-guard") {
		t.Fatalf("expected message to mention policy name pci-guard, got: %s", gwErr.Message)
	}

	// 2. Test prompt with SSN is masked in-place
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx2 := pipeline.NewRequestContext(context.Background(), w2, r2)
	reqCtx2.ChatRequest = &model.CanonicalChatRequest{
		Messages: []model.ChatMessage{
			{Role: "user", Content: "My SSN is 123-45-6789 please record."},
		},
	}
	reqCtx2.Policies = []*config.PolicyConfig{policies[1]} // only ssn-redact

	err2 := stage.Execute(reqCtx2)
	if err2 != nil {
		t.Fatalf("unexpected error for mask policy: %v", err2)
	}
	expectedContent := "My SSN is [REDACTED:ssn-redact] please record."
	if reqCtx2.ChatRequest.Messages[0].ContentString() != expectedContent {
		t.Fatalf("expected message content to be redacted, got: %s", reqCtx2.ChatRequest.Messages[0].ContentString())
	}
}

func TestPostPolicyStage(t *testing.T) {
	eng := NewEngine()
	stage := NewPostPolicyStage(eng)

	policy := []*config.PolicyConfig{
		{
			ID:       "pci-block",
			Name:     "pci-block",
			Action:   "BLOCK",
			Patterns: []string{"credit_card"},
		},
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := pipeline.NewRequestContext(context.Background(), w, r)
	reqCtx.Policies = policy
	reqCtx.ChatResponse = &model.CanonicalChatResponse{
		Choices: []model.ChatChoice{
			{
				Index: 0,
				Message: model.ChatMessage{
					Role:    "assistant",
					Content: "Here is your number: 4242-4242-4242-4242",
				},
			},
		},
		Usage: &model.UsageInfo{TotalTokens: 25},
	}

	reconciledTokens := -1
	reqCtx.Reconcile = func(tokens int) {
		reconciledTokens = tokens
	}

	err := stage.Execute(reqCtx)
	if err == nil {
		t.Fatal("expected post policy to block response with credit card")
	}
	if reconciledTokens != 0 {
		t.Fatalf("expected 0 tokens reconciled on block, got %d", reconciledTokens)
	}
}
