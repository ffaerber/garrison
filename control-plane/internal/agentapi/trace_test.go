package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/sandbox"
)

type fakeTracer struct {
	mu     sync.Mutex
	began  events.Trace
	steps  []events.Step
	status string
	calls  int
}

func (f *fakeTracer) Begin(_ context.Context, t events.Trace) int64 { f.began = t; return 7 }
func (f *fakeTracer) Add(_ context.Context, id int64, st ...events.Step) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, st...)
}
func (f *fakeTracer) End(_ context.Context, id int64, status string, calls int) {
	f.status, f.calls = status, calls
}

type agentID string

func (a agentID) ID() string { return string(a) }

func delta(kind, text string) sandbox.Event {
	var ev sandbox.Event
	json.Unmarshal([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"`+kind+`","delta":`+mustJSON(text)+`}}`), &ev)
	return ev
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestATurnsTrace(t *testing.T) {
	f := &fakeTracer{}
	s := &Server{Traces: f}
	tr := s.startTrace(context.Background(), agentID("todo/designer"), "todo", "bob", "channel:abc", sandbox.Run{StartedBy: "person"}, "make a short")
	if f.began.Agent != "todo/designer" || !f.began.Shared || f.began.Task != "make a short" {
		t.Fatalf("began %+v", f.began)
	}
	var start, end sandbox.Event
	json.Unmarshal([]byte(`{"type":"tool_execution_start","toolName":"generate_media","args":{"style":"soft"}}`), &start)
	json.Unmarshal([]byte(`{"type":"tool_execution_end","toolName":"generate_media","isError":true,"result":{"content":[{"type":"text","text":"ComfyUI refused"}]}}`), &end)
	for _, ev := range []sandbox.Event{delta("thinking_delta", "First the "), delta("thinking_delta", "narration."), delta("text_delta", "Making it."), start, end, delta("text_delta", "It failed.")} {
		tr.event(ev)
	}
	tr.end(errors.New("ComfyUI is away"), 1)
	want := []events.Step{
		{Kind: events.StepThinking, Text: "First the narration."},
		{Kind: events.StepSays, Text: "Making it."},
		{Kind: events.StepTool, Tool: "generate_media", Text: `{"style":"soft"}`},
		{Kind: events.StepResult, Tool: "generate_media", Text: "ComfyUI refused", Error: true},
		{Kind: events.StepSays, Text: "It failed."},
		{Kind: events.StepError, Text: "ComfyUI is away"},
	}
	if len(f.steps) != len(want) {
		t.Fatalf("steps %+v", f.steps)
	}
	for i := range want {
		if f.steps[i] != want[i] {
			t.Errorf("step %d: %+v, want %+v", i, f.steps[i], want[i])
		}
	}
	if f.status != events.StatusFailed || f.calls != 1 {
		t.Errorf("ended %s with %d calls", f.status, f.calls)
	}
	// A private chat's turn is the person's own; without a tracer, nothing.
	s.startTrace(context.Background(), agentID("todo/coder"), "todo", "bob", "local:1", sandbox.Run{}, "x")
	if f.began.Shared {
		t.Error("a private chat's turn is shared")
	}
	// In a channel, Pi's prompt carries the thread around the message; the
	// trace is titled with the person's words alone.
	s.startTrace(context.Background(), agentID("todo/designer"), "todo", "bob", "channel:abc", sandbox.Run{Task: "make a short"}, "Context from the chat:\n…\n\nmake a short")
	if f.began.Task != "make a short" {
		t.Errorf("titled %q", f.began.Task)
	}
	var none *tracing
	none.event(start)
	none.end(nil, 0)
	if (&Server{}).startTrace(context.Background(), agentID("a/b"), "a", "b", "c", sandbox.Run{}, "") != nil {
		t.Error("a trace without a tracer")
	}
}
