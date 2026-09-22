package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/company/ai-gateway/internal/model"
)

// GenesisHash is the cryptographic anchor for the genesis block (64 zeros).
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Record models a tamper-evident audit ledger entry per docs/SECURITY-BOUNDARIES.md §6.
type Record struct {
	Timestamp        string  `json:"timestamp"`
	RequestID        string  `json:"request_id"`
	TraceID          string  `json:"trace_id"`
	TenantID         string  `json:"tenant_id"`
	Route            string  `json:"route"`
	UpstreamID       string  `json:"upstream_id"`
	Model            string  `json:"model"`
	PromptHash       string  `json:"prompt_hash"`
	ResponseHash     string  `json:"response_hash"`
	TokensPrompt     int     `json:"tokens_prompt"`
	TokensCompletion int     `json:"tokens_completion"`
	StatusCode       int     `json:"status_code"`
	PolicyAction     string  `json:"policy_action"`
	OverheadMs       float64 `json:"overhead_ms"`
	PrevHash         string  `json:"prev_hash"`
	RecordHash       string  `json:"record_hash"`
}

// ComputeRecordHash calculates the cryptographic hash of an audit record:
// H_i = SHA-256(H_{i-1} || Timestamp || RequestID || TraceID || TenantID || Route || UpstreamID || Model || PromptHash || ResponseHash || StatusCode)
func ComputeRecordHash(prevHash, timestamp, requestID, traceID, tenantID, route, upstreamID, model, promptHash, responseHash string, statusCode int) string {
	var b strings.Builder
	b.WriteString(prevHash)
	b.WriteString(timestamp)
	b.WriteString(requestID)
	b.WriteString(traceID)
	b.WriteString(tenantID)
	b.WriteString(route)
	b.WriteString(upstreamID)
	b.WriteString(model)
	b.WriteString(promptHash)
	b.WriteString(responseHash)
	b.WriteString(strconv.Itoa(statusCode))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Ledger provides a thread-safe, append-only cryptographically chained audit log.
type Ledger struct {
	mu       sync.RWMutex
	lastHash string
	writer   io.Writer
	records  []Record
}

// NewLedger initializes a new Ledger instance anchored at GenesisHash.
func NewLedger(writer io.Writer) *Ledger {
	return &Ledger{
		lastHash: GenesisHash,
		writer:   writer,
		records:  make([]Record, 0),
	}
}

// Append thread-safely appends a record to the cryptographic ledger.
// It populates Timestamp (if empty), PrevHash, and RecordHash.
func (l *Ledger) Append(rec Record) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if rec.Timestamp == "" {
		rec.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	rec.PrevHash = l.lastHash
	rec.RecordHash = ComputeRecordHash(
		rec.PrevHash,
		rec.Timestamp,
		rec.RequestID,
		rec.TraceID,
		rec.TenantID,
		rec.Route,
		rec.UpstreamID,
		rec.Model,
		rec.PromptHash,
		rec.ResponseHash,
		rec.StatusCode,
	)

	if l.writer != nil {
		data, err := json.Marshal(rec)
		if err != nil {
			return Record{}, fmt.Errorf("failed to marshal audit record: %w", err)
		}
		data = append(data, '\n')
		if _, err := l.writer.Write(data); err != nil {
			return Record{}, fmt.Errorf("failed to write audit record to log sink: %w", err)
		}
	}

	l.records = append(l.records, rec)
	l.lastHash = rec.RecordHash

	return rec, nil
}

// Records returns a copy of all records stored in memory.
func (l *Ledger) Records() []Record {
	l.mu.RLock()
	defer l.mu.RUnlock()

	res := make([]Record, len(l.records))
	copy(res, l.records)
	return res
}

// LastHash returns the current tip of the cryptographic hash chain.
func (l *Ledger) LastHash() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.lastHash
}

// VerifyChain scans a reader containing JSON lines of audit records and verifies
// every link in the cryptographic hash chain.
// Returns (true, count, nil) if valid, or (false, index, error) on tampering/reordering/deletion.
func VerifyChain(reader io.Reader) (bool, int, error) {
	scanner := bufio.NewScanner(reader)
	// Allow up to 1MB per line for metadata/schema buffers
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	expectedPrevHash := GenesisHash
	count := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return false, count, fmt.Errorf("record %d JSON parse error: %w", count, err)
		}

		// 1. Verify previous hash chaining
		if count == 0 {
			if rec.PrevHash != GenesisHash {
				return false, 0, fmt.Errorf("genesis record prev_hash mismatch: expected %s, got %s", GenesisHash, rec.PrevHash)
			}
		} else {
			if rec.PrevHash != expectedPrevHash {
				return false, count, fmt.Errorf("record %d prev_hash mismatch: expected %s, got %s (chain link broken: records deleted, added, or reordered)", count, expectedPrevHash, rec.PrevHash)
			}
		}

		// 2. Verify record hash integrity
		calculatedHash := ComputeRecordHash(
			rec.PrevHash,
			rec.Timestamp,
			rec.RequestID,
			rec.TraceID,
			rec.TenantID,
			rec.Route,
			rec.UpstreamID,
			rec.Model,
			rec.PromptHash,
			rec.ResponseHash,
			rec.StatusCode,
		)

		if rec.RecordHash != calculatedHash {
			return false, count, fmt.Errorf("record %d record_hash mismatch: computed %s, record specifies %s (record tampered)", count, calculatedHash, rec.RecordHash)
		}

		expectedPrevHash = rec.RecordHash
		count++
	}

	if err := scanner.Err(); err != nil {
		return false, count, fmt.Errorf("scanner error reading audit chain: %w", err)
	}

	return true, count, nil
}

// HashString returns the SHA-256 hex string of input text.
func HashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashPrompt calculates the SHA-256 digest of chat messages without exposing raw PII.
func HashPrompt(messages []model.ChatMessage) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString(m.Role)
		b.WriteString(":")
		b.WriteString(m.ContentString())
		b.WriteString("\n")
	}
	return HashString(b.String())
}

// HashResponse calculates the SHA-256 digest of completion response text.
func HashResponse(resp *model.CanonicalChatResponse) string {
	if resp == nil {
		return HashString("")
	}
	var b strings.Builder
	for _, c := range resp.Choices {
		b.WriteString(c.Message.ContentString())
	}
	return HashString(b.String())
}
