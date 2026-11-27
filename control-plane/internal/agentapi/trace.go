package agentapi

import (
	"context"
	"strings"
	"sync"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/sandbox"
)

// tracing records one turn's trace for the dashboard: thinking and text as
// whole pieces (gathered from their deltas), each tool call with its
// arguments and its result. Steps go to the store in order, from one
// goroutine, so that a slow database never holds up the turn.
type tracing struct {
	t  events.Tracer
	id int64

	mu       sync.Mutex
	thinking strings.Builder
	says     strings.Builder
	ch       chan events.Step
	done     chan struct{}
}

// startTrace begins the trace of a turn of a for person in chat; nil
// without a tracer, or if it cannot be recorded.
func (s *Server) startTrace(ctx context.Context, a interface{ ID() string }, unit, person, chat string, run sandbox.Run, task string) *tracing {
	if s.Traces == nil {
		return nil
	}
	if strings.TrimSpace(run.Task) != "" {
		task = run.Task // the person's words, not the chat's context around them
	}
	id := s.Traces.Begin(ctx, events.Trace{
		Unit: unit, Agent: a.ID(), Person: person, Chat: chat, StartedBy: run.StartedBy,
		Shared: events.Shared(chat), Task: task,
	})
	if id == 0 {
		return nil
	}
	tr := &tracing{t: s.Traces, id: id, ch: make(chan events.Step, 256), done: make(chan struct{})}
	go func() {
		defer close(tr.done)
		bg := context.WithoutCancel(ctx)
		for st := range tr.ch {
			tr.t.Add(bg, tr.id, st)
		}
	}()
	return tr
}

// event takes one of Pi's events.
func (tr *tracing) event(ev sandbox.Event) {
	if tr == nil {
		return
	}
	switch ev.Type {
	case "message_update":
		if ev.Delta == nil {
			return
		}
		tr.mu.Lock()
		switch ev.Delta.Type {
		case "thinking_delta":
			tr.thinking.WriteString(ev.Delta.Delta)
		case "text_delta":
			tr.flushThinking()
			tr.says.WriteString(ev.Delta.Delta)
		}
		tr.mu.Unlock()
	case "message_end":
		tr.mu.Lock()
		tr.flushThinking()
		if tr.says.Len() == 0 && ev.Message != nil && ev.Message.Role == "assistant" {
			tr.says.WriteString(ev.Message.Text()) // a provider that does not stream
		}
		tr.flushSays()
		tr.mu.Unlock()
	case "tool_execution_start":
		tr.mu.Lock()
		tr.flushThinking()
		tr.flushSays()
		tr.mu.Unlock()
		tr.send(events.Step{Kind: events.StepTool, Tool: ev.ToolName, Text: string(ev.Args)})
	case "tool_execution_end":
		var b strings.Builder
		if ev.Result != nil {
			for _, c := range ev.Result.Content {
				b.WriteString(c.Text)
			}
		}
		tr.send(events.Step{Kind: events.StepResult, Tool: ev.ToolName, Text: b.String(), Error: ev.IsError})
	case "auto_retry_start":
		tr.send(events.Step{Kind: events.StepError, Text: "The LLM failed (" + ev.ErrorMessage + "); retrying."})
	}
}

// flushThinking and flushSays send what was gathered; tr.mu is held.
func (tr *tracing) flushThinking() {
	if t := strings.TrimSpace(tr.thinking.String()); t != "" {
		tr.send(events.Step{Kind: events.StepThinking, Text: t})
	}
	tr.thinking.Reset()
}

func (tr *tracing) flushSays() {
	if t := strings.TrimSpace(tr.says.String()); t != "" {
		tr.send(events.Step{Kind: events.StepSays, Text: t})
	}
	tr.says.Reset()
}

func (tr *tracing) send(st events.Step) {
	select {
	case tr.ch <- st:
	default: // the store is far behind: this step is left out
	}
}

// end records how the turn ended, after the steps still on their way.
func (tr *tracing) end(err error, calls int) {
	if tr == nil {
		return
	}
	tr.mu.Lock()
	tr.flushThinking()
	tr.flushSays()
	tr.mu.Unlock()
	status := events.StatusOK
	if err != nil {
		status = events.StatusFailed
		tr.send(events.Step{Kind: events.StepError, Text: err.Error()})
	}
	close(tr.ch)
	<-tr.done
	tr.t.End(context.Background(), tr.id, status, calls)
}
