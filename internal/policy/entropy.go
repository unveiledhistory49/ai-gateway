package policy

import (
	"math"
	"regexp"
)

// candidateTokenRegex matches contiguous secret-like tokens of length >= 24 without whitespace or control chars.
var candidateTokenRegex = regexp.MustCompile(`[A-Za-z0-9+/=_\-\.]{24,}`)

// ShannonEntropy calculates the Shannon information entropy of a string in bits per symbol:
// H(X) = - \sum P(x_i) * log2(P(x_i))
func ShannonEntropy(s string) float64 {
	if len(s) == 0 {
		return 0.0
	}

	freq := make(map[rune]int)
	total := 0
	for _, ch := range s {
		freq[ch]++
		total++
	}

	totalF := float64(total)
	var entropy float64
	for _, count := range freq {
		p := float64(count) / totalF
		if p > 0 {
			entropy -= p * math.Log2(p)
		}
	}

	return entropy
}

// FindHighEntropySpans identifies substrings of length >= 24 with Shannon entropy >= 4.5.
func FindHighEntropySpans(text string, minLen int, threshold float64) [][]int {
	if minLen <= 0 {
		minLen = 24
	}
	if threshold <= 0 {
		threshold = 4.5
	}

	var results [][]int
	indices := candidateTokenRegex.FindAllStringIndex(text, -1)
	for _, idx := range indices {
		start, end := idx[0], idx[1]
		sub := text[start:end]
		if len(sub) >= minLen {
			ent := ShannonEntropy(sub)
			if ent >= threshold {
				results = append(results, []int{start, end})
			}
		}
	}

	return results
}
