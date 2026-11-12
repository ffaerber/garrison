package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Pi talks to Pi's RPC mode (pi --mode rpc): commands as JSON lines on its
// stdin; responses, which carry the command's id, and events as JSON lines
// on its stdout.
type Pi struct {
	in     io.WriteCloser
	out    io.ReadCloser
	events chan Event
	inTurn atomic.Bool

	mu      sync.Mutex
	next    int
	waiting map[string]chan Event
	done    chan struct{}
	err     error
}

// Event is a response or an event of Pi's, with the fields used here.
type Event struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`

	Message *Message `json:"message"` // message_start, message_end, turn_end
	Delta   *struct {
		Type  string `json:"type"` // text_delta, thinking_delta...
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"` // message_update

	ToolName string          `json:"toolName"` // tool_execution_*
	Args     json.RawMessage `json:"args"`
	IsError  bool            `json:"isError"`
	Result   *struct {
		Content []Content `json:"content"`
	} `json:"result"`

	ErrorMessage string `json:"errorMessage"` // auto_retry_start
	FinalError   string `json:"finalError"`   // auto_retry_end
}

// Message is an AgentMessage: a user's, the assistant's or a tool result.
type Message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"` // a string or parts
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
}

// Content is a part of a message.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Text is the text of a message, without thinking and tool calls.
func (m *Message) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []Content
	json.Unmarshal(m.Content, &parts)
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Image goes along with a prompt.
type Image struct {
	Type     string `json:"type"` // image
	Data     string `json:"data"` // base64
	MimeType string `json:"mimeType"`
}

// NewPi reads Pi's stdout until it ends.
func NewPi(in io.WriteCloser, out io.ReadCloser) *Pi {
	p := &Pi{in: in, out: out, events: make(chan Event, 4096), waiting: map[string]chan Event{}, done: make(chan struct{})}
	go p.read()
	return p
}

func (p *Pi) read() {
	// Records end at \n only: JSON strings may hold other line separators.
	r := bufio.NewReaderSize(p.out, 64<<10)
	var err error
	for {
		var line []byte
		line, err = r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var ev Event
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				log.Printf("sandbox: not a Pi record: %.200s", line)
			} else {
				p.deliver(ev)
			}
		}
		if err != nil {
			break
		}
	}
	if err == io.EOF {
		err = errors.New("pi stopped")
	}
	p.mu.Lock()
	p.err = err
	p.mu.Unlock()
	close(p.done)
}

func (p *Pi) deliver(ev Event) {
	if ev.Type == "response" && ev.ID != "" {
		p.mu.Lock()
		ch := p.waiting[ev.ID]
		delete(p.waiting, ev.ID)
		p.mu.Unlock()
		if ch != nil {
			ch <- ev
			return
		}
	}
	if p.inTurn.Load() {
		select {
		case p.events <- ev:
		case <-time.After(time.Minute):
			log.Printf("sandbox: dropped a Pi event nobody read: %s", ev.Type)
		}
		return
	}
	// Between turns nothing reads events; they are not needed.
}

// Done is closed when Pi has stopped.
func (p *Pi) Done() <-chan struct{} { return p.done }

// Err is why Pi stopped.
func (p *Pi) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Close ends Pi's stdin and the connection.
func (p *Pi) Close() error {
	p.in.Close()
	return p.out.Close()
}

func (p *Pi) send(cmd map[string]any) (chan Event, string, error) {
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("hc-%d", p.next)
	ch := make(chan Event, 1)
	p.waiting[id] = ch
	p.mu.Unlock()
	cmd["id"] = id
	b, _ := json.Marshal(cmd)
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return nil, "", fmt.Errorf("pi: %w", err)
	}
	return ch, id, nil
}

// Command sends a command and waits for its response.
func (p *Pi) Command(ctx context.Context, cmd map[string]any) (Event, error) {
	ch, id, err := p.send(cmd)
	if err != nil {
		return Event{}, err
	}
	select {
	case ev := <-ch:
		if ev.Success != nil && !*ev.Success {
			return ev, fmt.Errorf("pi: %s: %s", cmd["type"], ev.Error)
		}
		return ev, nil
	case <-p.done:
		return Event{}, fmt.Errorf("pi: %w", p.Err())
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		return Event{}, ctx.Err()
	}
}

// State is part of get_state's answer.
type State struct {
	MessageCount int  `json:"messageCount"`
	IsStreaming  bool `json:"isStreaming"`
}

func (p *Pi) State(ctx context.Context) (State, error) {
	ev, err := p.Command(ctx, map[string]any{"type": "get_state"})
	var s State
	if err == nil {
		err = json.Unmarshal(ev.Data, &s)
	}
	return s, err
}

// Prompt runs one turn: it sends the message and hands each event to on
// until the agent ends. When ctx ends first, the turn is aborted.
func (p *Pi) Prompt(ctx context.Context, message string, images []Image, on func(Event)) error {
	// Events left from an aborted turn are not this turn's.
	for len(p.events) > 0 {
		<-p.events
	}
	p.inTurn.Store(true)
	defer p.inTurn.Store(false)
	cmd := map[string]any{"type": "prompt", "message": message}
	if len(images) > 0 {
		cmd["images"] = images
	}
	if _, err := p.Command(ctx, cmd); err != nil {
		return err
	}
	var aborted <-chan time.Time
	stop := ctx.Done()
	for {
		select {
		case ev := <-p.events:
			on(ev)
			if ev.Type == "agent_end" {
				if aborted != nil {
					return ctx.Err()
				}
				return nil
			}
		case <-p.done:
			return fmt.Errorf("pi: %w", p.Err())
		case <-stop:
			// Ask Pi to stop, and wait a little for it to end the turn.
			stop = nil
			aborted = time.After(30 * time.Second)
			abort, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			p.Command(abort, map[string]any{"type": "abort"})
			cancel()
		case <-aborted:
			return ctx.Err()
		}
	}
}
