package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type mockStage struct {
	name      string
	executeFn func(ctx *RequestContext) error
}

func (m *mockStage) Name() string {
	return m.name
}

func (m *mockStage) Execute(ctx *RequestContext) error {
	if m.executeFn != nil {
		return m.executeFn(ctx)
	}
	return nil
}

func TestPipelineSequentialExecution(t *testing.T) {
	var executed []string

	s1 := &mockStage{name: "stage1", executeFn: func(ctx *RequestContext) error {
		executed = append(executed, "stage1")
		time.Sleep(2 * time.Millisecond)
		return nil
	}}
	s2 := &mockStage{name: "stage2", executeFn: func(ctx *RequestContext) error {
		executed = append(executed, "stage2")
		return nil
	}}

	p := NewPipeline(s1, s2)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := NewRequestContext(context.Background(), w, r)

	err := p.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected pipeline error: %v", err)
	}

	if len(executed) != 2 || executed[0] != "stage1" || executed[1] != "stage2" {
		t.Fatalf("expected stages to execute in order, got %v", executed)
	}

	if d, ok := reqCtx.StageDurations["stage1"]; !ok || d < 2*time.Millisecond {
		t.Errorf("expected stage1 duration to be recorded >= 2ms, got %v", d)
	}
	if _, ok := reqCtx.StageDurations["stage2"]; !ok {
		t.Errorf("expected stage2 duration to be recorded")
	}
}

func TestPipelineShortCircuit(t *testing.T) {
	var executed []string

	s1 := &mockStage{name: "stage1", executeFn: func(ctx *RequestContext) error {
		executed = append(executed, "stage1")
		ctx.ShortCircuited = true
		return nil
	}}
	s2 := &mockStage{name: "stage2", executeFn: func(ctx *RequestContext) error {
		executed = append(executed, "stage2")
		return nil
	}}

	p := NewPipeline(s1, s2)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := NewRequestContext(context.Background(), w, r)

	err := p.Execute(reqCtx)
	if err != nil {
		t.Fatalf("unexpected pipeline error: %v", err)
	}

	if len(executed) != 1 || executed[0] != "stage1" {
		t.Fatalf("expected only stage1 to execute before short-circuit, got %v", executed)
	}
}

func TestPipelineErrorAborts(t *testing.T) {
	expectedErr := errors.New("stage1 failed fatally")

	s1 := &mockStage{name: "stage1", executeFn: func(ctx *RequestContext) error {
		return expectedErr
	}}
	s2 := &mockStage{name: "stage2", executeFn: func(ctx *RequestContext) error {
		t.Fatal("stage2 should not have executed")
		return nil
	}}

	p := NewPipeline(s1, s2)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := NewRequestContext(context.Background(), w, r)

	err := p.Execute(reqCtx)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected %v, got %v", expectedErr, err)
	}
}

func TestPipelineContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before pipeline execution

	s1 := &mockStage{name: "stage1", executeFn: func(reqCtx *RequestContext) error {
		t.Fatal("stage1 should not execute when context is canceled")
		return nil
	}}

	p := NewPipeline(s1)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqCtx := NewRequestContext(ctx, w, r)

	err := p.Execute(reqCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
