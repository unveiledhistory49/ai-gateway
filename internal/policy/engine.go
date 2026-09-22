package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/company/ai-gateway/internal/config"
)

// Precompiled linear-time RE2 regular expressions for deterministic scanning.
var (
	// panRegex matches potential credit card numbers (13 to 19 digits, with optional spaces or dashes).
	panRegex = regexp.MustCompile(`\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|3[47][0-9]{13}|4[0-9]{3}[- ]?[0-9]{4}[- ]?[0-9]{4}[- ]?[0-9]{4}|5[1-5][0-9]{2}[- ]?[0-9]{4}[- ]?[0-9]{4}[- ]?[0-9]{4}|3[47][0-9]{2}[- ]?[0-9]{6}[- ]?[0-9]{5}|\d{4}[- ]?\d{4}[- ]?\d{4}[- ]?\d{4}|\d{13,19})\b`)

	// awsSecretRegex matches AWS secret access keys per specification: (?i)aws(.{0,20})?['\"][0-9a-zA-Z/+]{40}['\"]
	awsSecretRegex = regexp.MustCompile(`(?i)aws(.{0,20})?['\"][0-9a-zA-Z/+]{40}['\"]`)

	// awsAccessKeyRegex matches AWS Access Key IDs: AKIA, ASIA, etc.
	awsAccessKeyRegex = regexp.MustCompile(`\b(AKIA|ASIA|AROA|AIPA)[A-Z0-9]{16}\b`)

	// privateKeyRegex matches PEM private key headers per specification
	privateKeyRegex = regexp.MustCompile(`-----BEGIN (?:(?:RSA|EC|DSA|OPENSSH) )?(?:PRIVATE )?KEY-----`)
)

// MatchSpan represents a detected pattern range in text.
type MatchSpan struct {
	Start      int
	End        int
	PolicyID   string
	PolicyName string
	Action     string
	Rule       string
}

// ScanResult contains the outcome of evaluating DLP policies.
type ScanResult struct {
	Violated       bool
	Action         string // "BLOCK" or "MASK"
	ViolatedPolicy string // Name or ID of the violated policy
	ViolatedRule   string // Rule pattern that triggered violation
	RedactedText   string
	Matches        []MatchSpan
}

// Engine encapsulates deterministic policy scanning.
type Engine struct{}

// NewEngine creates a new deterministic Policy Engine.
func NewEngine() *Engine {
	return &Engine{}
}

// Scan evaluates text against the provided list of policies.
func (e *Engine) Scan(text string, policies []*config.PolicyConfig) (*ScanResult, error) {
	if len(policies) == 0 || len(text) == 0 {
		return &ScanResult{
			Violated:     false,
			RedactedText: text,
		}, nil
	}

	var allMatches []MatchSpan

	for _, policy := range policies {
		policyName := policy.Name
		if policyName == "" {
			policyName = policy.ID
		}

		action := strings.ToUpper(policy.Action)
		if action == "REDACT" {
			action = "MASK"
		}

		for _, pat := range policy.AllPatterns() {
			normalizedPat := strings.ToLower(strings.TrimSpace(pat))

			switch normalizedPat {
			case "credit_card", "pci_credit_card", "pan":
				spans := findCreditCardSpans(text)
				for _, span := range spans {
					allMatches = append(allMatches, MatchSpan{
						Start:      span[0],
						End:        span[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "credit_card",
					})
				}

			case "ssn", "us_ssn":
				spans := FindSSNSpans(text)
				for _, span := range spans {
					allMatches = append(allMatches, MatchSpan{
						Start:      span[0],
						End:        span[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "ssn",
					})
				}

			case "aws_key", "aws_secret_key", "aws":
				// Scan for AWS secret access keys
				indices := awsSecretRegex.FindAllStringIndex(text, -1)
				for _, idx := range indices {
					allMatches = append(allMatches, MatchSpan{
						Start:      idx[0],
						End:        idx[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "aws_key",
					})
				}
				// Scan for AWS access key IDs
				idIndices := awsAccessKeyRegex.FindAllStringIndex(text, -1)
				for _, idx := range idIndices {
					allMatches = append(allMatches, MatchSpan{
						Start:      idx[0],
						End:        idx[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "aws_key",
					})
				}

			case "private_key", "pem_key":
				indices := privateKeyRegex.FindAllStringIndex(text, -1)
				for _, idx := range indices {
					allMatches = append(allMatches, MatchSpan{
						Start:      idx[0],
						End:        idx[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "private_key",
					})
				}

			case "high_entropy", "shannon_entropy", "entropy":
				spans := FindHighEntropySpans(text, 24, 4.5)
				for _, span := range spans {
					allMatches = append(allMatches, MatchSpan{
						Start:      span[0],
						End:        span[1],
						PolicyID:   policy.ID,
						PolicyName: policyName,
						Action:     action,
						Rule:       "high_entropy",
					})
				}

			default:
				// If pattern looks like a custom regex
				if strings.HasPrefix(pat, "regex:") {
					customPattern := strings.TrimPrefix(pat, "regex:")
					if re, err := regexp.Compile(customPattern); err == nil {
						indices := re.FindAllStringIndex(text, -1)
						for _, idx := range indices {
							allMatches = append(allMatches, MatchSpan{
								Start:      idx[0],
								End:        idx[1],
								PolicyID:   policy.ID,
								PolicyName: policyName,
								Action:     action,
								Rule:       pat,
							})
						}
					}
				}
			}
		}
	}

	if len(allMatches) == 0 {
		return &ScanResult{
			Violated:     false,
			RedactedText: text,
		}, nil
	}

	// Check if any match has action "BLOCK"
	for _, m := range allMatches {
		if m.Action == "BLOCK" {
			return &ScanResult{
				Violated:       true,
				Action:         "BLOCK",
				ViolatedPolicy: m.PolicyName,
				ViolatedRule:   m.Rule,
				RedactedText:   text,
				Matches:        allMatches,
			}, nil
		}
	}

	// All matches have action "MASK" - redact matches from right to left
	// Deduplicate overlapping spans
	mergedSpans := mergeSpans(allMatches)

	// Sort spans in descending order of start index
	sort.Slice(mergedSpans, func(i, j int) bool {
		return mergedSpans[i].Start > mergedSpans[j].Start
	})

	redacted := text
	for _, m := range mergedSpans {
		replacement := fmt.Sprintf("[REDACTED:%s]", m.PolicyName)
		redacted = redacted[:m.Start] + replacement + redacted[m.End:]
	}

	return &ScanResult{
		Violated:       true,
		Action:         "MASK",
		ViolatedPolicy: mergedSpans[0].PolicyName,
		ViolatedRule:   mergedSpans[0].Rule,
		RedactedText:   redacted,
		Matches:        allMatches,
	}, nil
}

func findCreditCardSpans(text string) [][]int {
	matches := panRegex.FindAllStringIndex(text, -1)
	var validSpans [][]int
	for _, m := range matches {
		candidate := text[m[0]:m[1]]
		if ValidateLuhn(candidate) {
			validSpans = append(validSpans, []int{m[0], m[1]})
		}
	}
	return validSpans
}

func mergeSpans(matches []MatchSpan) []MatchSpan {
	if len(matches) <= 1 {
		return matches
	}

	// Sort by start index ascending
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Start == matches[j].Start {
			return matches[i].End > matches[j].End
		}
		return matches[i].Start < matches[j].Start
	})

	merged := make([]MatchSpan, 0, len(matches))
	current := matches[0]

	for i := 1; i < len(matches); i++ {
		next := matches[i]
		if next.Start <= current.End {
			// Overlapping or adjacent
			if next.End > current.End {
				current.End = next.End
			}
		} else {
			merged = append(merged, current)
			current = next
		}
	}
	merged = append(merged, current)
	return merged
}
