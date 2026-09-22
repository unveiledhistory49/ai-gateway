package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/company/ai-gateway/internal/model"
)

func TestLedgerGenesisAndChaining(t *testing.T) {
	var buf bytes.Buffer
	ledger := NewLedger(&buf)

	if ledger.LastHash() != GenesisHash {
		t.Fatalf("expected initial lastHash to be GenesisHash, got %s", ledger.LastHash())
	}

	rec1, err := ledger.Append(Record{
		RequestID:    "req-1",
		TraceID:      "trace-1",
		TenantID:     "tenant-alpha",
		Route:        "/v1/chat/completions",
		UpstreamID:   "openai-1",
		Model:        "gpt-4o",
		PromptHash:   HashString("hello world"),
		ResponseHash: HashString("hi there"),
		StatusCode:   200,
		PolicyAction: "ALLOW",
		OverheadMs:   1.2,
	})
	if err != nil {
		t.Fatalf("failed to append rec1: %v", err)
	}

	if rec1.PrevHash != GenesisHash {
		t.Fatalf("expected rec1.PrevHash == GenesisHash, got %s", rec1.PrevHash)
	}
	if rec1.RecordHash == "" || rec1.RecordHash == GenesisHash {
		t.Fatalf("invalid rec1.RecordHash: %s", rec1.RecordHash)
	}
	if ledger.LastHash() != rec1.RecordHash {
		t.Fatalf("expected ledger.LastHash() == rec1.RecordHash")
	}

	rec2, err := ledger.Append(Record{
		RequestID:    "req-2",
		TraceID:      "trace-2",
		TenantID:     "tenant-beta",
		Route:        "/v1/chat/completions",
		UpstreamID:   "anthropic-1",
		Model:        "claude-3-5-sonnet",
		PromptHash:   HashString("another prompt"),
		ResponseHash: HashString("another response"),
		StatusCode:   200,
		PolicyAction: "ALLOW",
		OverheadMs:   2.5,
	})
	if err != nil {
		t.Fatalf("failed to append rec2: %v", err)
	}

	if rec2.PrevHash != rec1.RecordHash {
		t.Fatalf("expected rec2.PrevHash == rec1.RecordHash, got %s", rec2.PrevHash)
	}
	if ledger.LastHash() != rec2.RecordHash {
		t.Fatalf("expected ledger.LastHash() == rec2.RecordHash")
	}

	// Verify valid chain
	valid, count, err := VerifyChain(&buf)
	if !valid || err != nil || count != 2 {
		t.Fatalf("expected chain to verify successfully: valid=%v, count=%d, err=%v", valid, count, err)
	}
}

func TestVerifyChainTamperDetection(t *testing.T) {
	// Build a 5-record valid chain
	records := make([]Record, 5)
	prev := GenesisHash
	for i := 0; i < 5; i++ {
		rec := Record{
			Timestamp:    "2026-09-22T12:00:0" + string(rune('0'+i)) + ".000Z",
			RequestID:    "req-" + string(rune('0'+i)),
			TraceID:      "trace-" + string(rune('0'+i)),
			TenantID:     "tenant-test",
			Route:        "/v1/chat/completions",
			UpstreamID:   "upstream-1",
			Model:        "gpt-4o",
			PromptHash:   HashString("prompt"),
			ResponseHash: HashString("resp"),
			StatusCode:   200,
			PrevHash:     prev,
		}
		rec.RecordHash = ComputeRecordHash(
			rec.PrevHash, rec.Timestamp, rec.RequestID, rec.TraceID,
			rec.TenantID, rec.Route, rec.UpstreamID, rec.Model,
			rec.PromptHash, rec.ResponseHash, rec.StatusCode,
		)
		prev = rec.RecordHash
		records[i] = rec
	}

	serialize := func(recs []Record) *bytes.Buffer {
		var buf bytes.Buffer
		for _, r := range recs {
			data, _ := json.Marshal(r)
			buf.Write(data)
			buf.WriteByte('\n')
		}
		return &buf
	}

	// 1. Verify baseline works
	valid, count, err := VerifyChain(serialize(records))
	if !valid || err != nil || count != 5 {
		t.Fatalf("baseline chain verification failed: valid=%v, count=%d, err=%v", valid, count, err)
	}

	// 2. Modifying a field in record 2
	tamperedContent := make([]Record, len(records))
	copy(tamperedContent, records)
	tamperedContent[2].StatusCode = 500 // changed status code without updating hash
	valid, index, err := VerifyChain(serialize(tamperedContent))
	if valid || err == nil || index != 2 {
		t.Fatalf("expected tamper detection at index 2, got valid=%v, index=%d, err=%v", valid, index, err)
	}

	// 3. Deleting record 2 (chain broken between 1 and 3)
	deletedRecord := append([]Record{}, records[0:2]...)
	deletedRecord = append(deletedRecord, records[3:]...)
	valid, index, err = VerifyChain(serialize(deletedRecord))
	if valid || err == nil || index != 2 {
		t.Fatalf("expected broken chain detection at index 2, got valid=%v, index=%d, err=%v", valid, index, err)
	}

	// 4. Reordering records 1 and 2
	reordered := append([]Record{}, records...)
	reordered[1], reordered[2] = reordered[2], reordered[1]
	valid, index, err = VerifyChain(serialize(reordered))
	if valid || err == nil || index != 1 {
		t.Fatalf("expected reordering detection at index 1, got valid=%v, index=%d, err=%v", valid, index, err)
	}

	// 5. Tampering with genesis record prev_hash
	tamperedGenesis := append([]Record{}, records...)
	tamperedGenesis[0].PrevHash = "badgenesis" + strings.Repeat("0", 54)
	valid, index, err = VerifyChain(serialize(tamperedGenesis))
	if valid || err == nil || index != 0 {
		t.Fatalf("expected genesis error at index 0, got valid=%v, index=%d, err=%v", valid, index, err)
	}
}

func TestHashPromptAndResponse(t *testing.T) {
	msgs := []model.ChatMessage{
		{Role: "system", Content: "You are a helpful assistant"},
		{Role: "user", Content: "Hello world"},
	}

	h1 := HashPrompt(msgs)
	h2 := HashPrompt(msgs)
	if h1 != h2 {
		t.Fatalf("hash prompt must be deterministic: %s != %s", h1, h2)
	}

	resp := &model.CanonicalChatResponse{
		Choices: []model.ChatChoice{
			{Message: model.ChatMessage{Role: "assistant", Content: "Hi there"}},
		},
	}
	r1 := HashResponse(resp)
	r2 := HashResponse(resp)
	if r1 != r2 {
		t.Fatalf("hash response must be deterministic: %s != %s", r1, r2)
	}
}
