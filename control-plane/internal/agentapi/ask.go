package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// MaxChain is how many agents one run may hold: the one a person asked,
// and those it asks in turn.
const MaxChain = 3

// MaxAsks is how often one turn may ask the same agent: once, and twice
// again with a sharper task when its answer was not enough.
const MaxAsks = 3

// Statuses an asked agent ends its answer with.
const (
	StatusDone    = "done"
	StatusPartial = "partial"
	StatusStuck   = "stuck"
)

// statusLine is the asked agent's last line on how far it got, e.g.
// "Status: stuck: no page on the topic would load".
var statusLine = regexp.MustCompile(`(?im)^[\s*_>-]*status[*_]*\s*:[*_\s]*(done|partial|stuck)\b[*_]*\s*[:,.\x{2013}\x{2014}-]?\s*(.*)$`)

// Outcome is how far an asked agent got, by its own last status line: done,
// partial or stuck, and why. Status is "" when it gave none.
type Outcome struct {
	Status, Why string
}

// outcomeOf reads the last status line of an answer.
func outcomeOf(answer string) Outcome {
	all := statusLine.FindAllStringSubmatch(answer, -1)
	if len(all) == 0 {
		return Outcome{}
	}
	m := all[len(all)-1]
	return Outcome{Status: strings.ToLower(m[1]), Why: strings.TrimSpace(strings.Trim(m[2], "*_ "))}
}

// statusAsk is what the asked agent is told about its last line.
const statusAsk = "End your answer with one line on how far you got: \"Status: done\", \"Status: partial: <what is missing>\" or \"Status: stuck: <what blocked you, e.g. no results, a page that would not load, an unclear task, a tool or right you lack>\". Be honest: whoever asked you decides by it whether to ask again."

// retryHint is said to the asker after an answer that is not done: ask once
// more, sharper, while it may; else answer with what it has.
func retryHint(target string, o Outcome, n int) string {
	if o.Status == "" || o.Status == StatusDone {
		return ""
	}
	if n >= MaxAsks {
		return fmt.Sprintf("\n\n%s did not finish (%s), and you asked it %d times this turn, the most there may be. Answer with what you have and say plainly what is missing.", target, o.Status, n)
	}
	left := MaxAsks - n
	return fmt.Sprintf("\n\n%s did not finish (%s). If its answer is not enough, ask it once more with a sharper task: say what this answer lacked, and change what blocked it (other sources or ways, a narrower scope, a clearer format in done_when). Do not repeat the same task. You may ask it %d more time(s) this turn; otherwise answer with what you have and say what is missing.", target, o.Status, left)
}

// Poster posts an answer no message waits for into a channel, and tells
// what a channel is.
type Poster interface {
	Post(ctx context.Context, p gateway.Post) (gateway.Posted, error)
	Channel(ctx context.Context, id string) (gateway.ChannelInfo, error)
	// Thread is the thread under a message, oldest first.
	Thread(ctx context.Context, channel, message string) ([]gateway.ThreadMessage, error)
	// UnitChannel is the address of a unit's channel, "" while it has none.
	UnitChannel(ctx context.Context, unit string) (string, error)
}

// GatewayPoster posts through the gateway, which holds Open WebUI's account.
type GatewayPoster struct {
	URL        string // the gateway, e.g. http://gateway:8080
	ServiceKey string
	HTTP       *http.Client
}

func (g *GatewayPoster) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GatewayPoster) Channel(ctx context.Context, id string) (gateway.ChannelInfo, error) {
	var info gateway.ChannelInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/internal/channels/"+url.PathEscape(id), nil)
	if err != nil {
		return info, err
	}
	req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
	resp, err := g.client().Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("channel %s: %s", id, resp.Status)
	}
	return info, json.NewDecoder(resp.Body).Decode(&info)
}

func (g *GatewayPoster) UnitChannel(ctx context.Context, unit string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/internal/units/"+url.PathEscape(unit)+"/channel", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the channel of %s: %s", unit, resp.Status)
	}
	var out struct {
		URL string `json:"url"`
	}
	return out.URL, json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
}

func (g *GatewayPoster) Post(ctx context.Context, p gateway.Post) (gateway.Posted, error) {
	var out gateway.Posted
	body, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.URL+"/internal/post", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
	resp, err := g.client().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return out, fmt.Errorf("post: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return out, json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
}

func (g *GatewayPoster) Thread(ctx context.Context, channel, message string) ([]gateway.ThreadMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+"/internal/threads/"+url.PathEscape(channel)+"/"+url.PathEscape(message), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, fmt.Errorf("thread: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out []gateway.ThreadMessage
	return out, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out)
}

// collect gathers an asked agent's answer, showing its progress, line by
// line under its name, to whoever waits.
type collect struct {
	mu     sync.Mutex
	show   func(string)
	prefix string
	inLine bool
	text   strings.Builder
}

func (c *collect) reasoning(s string) {
	if c.show == nil || s == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if line == "" {
			continue
		}
		if !c.inLine {
			b.WriteString(c.prefix)
		}
		b.WriteString(line)
		c.inLine = !strings.HasSuffix(line, "\n")
	}
	c.show(b.String())
}

func (c *collect) content(s string) {
	c.mu.Lock()
	c.text.WriteString(s)
	c.mu.Unlock()
}

// step: the text so far was a step before a tool call. It shows as
// progress; the answer is the last message alone.
func (c *collect) step() {
	c.mu.Lock()
	t := strings.TrimSpace(c.text.String())
	c.text.Reset()
	c.mu.Unlock()
	if t != "" {
		c.reasoning(t + "\n")
	}
}

func (c *collect) answer() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.text.String())
}

// askSpec is the tool ask_agent, for an agent that may ask others.
func askSpec(a agents.Agent) toolSpec {
	agent := map[string]any{"type": "string", "enum": a.Asks, "description": "the agent to ask; it works in your unit"}
	if a.AsksAny() {
		agent = map[string]any{"type": "string", "description": "the agent to ask, by name as find_agents lists it; it works in your unit. For one working in another unit: <unit>/<agent>"}
	}
	return toolSpec{
		Name:        "ask_agent",
		Description: fmt.Sprintf("Hands work to another agent and returns its answer, which ends with its status: done, partial or stuck, and why. It works for the same person, with its own tools and rights, in a workspace of its own. If it is partial or stuck, you may ask it again with a sharper task, up to %d times in all per turn. With background, it works on after this turn and posts its answer in this channel instead (only in a channel).", MaxAsks),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent":      agent,
				"task":       map[string]any{"type": "string", "description": "what it should do, with what it needs to know, e.g. the repo and branch"},
				"limits":     map[string]any{"type": "string", "description": "what bounds it: what not to do, how far to go"},
				"done_when":  map[string]any{"type": "string", "description": "what a good answer looks like"},
				"background": map[string]any{"type": "boolean", "description": "do not wait; it posts its answer in the channel"},
			},
			"required":             []string{"agent", "task", "done_when"},
			"additionalProperties": false,
		},
	}
}

// askAgent runs ask_agent for the sandbox c: a turn of the asked agent, for
// the same person, in its own sandbox of the same chat.
func (s *Server) askAgent(ctx context.Context, c sandbox.Caller, raw json.RawMessage) (string, bool) {
	var in struct {
		Agent      string `json:"agent"`
		Task       string `json:"task"`
		Limits     string `json:"limits"`
		DoneWhen   string `json:"done_when"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Task) == "" {
		return "error: ask_agent needs agent, task and done_when", true
	}
	asker, ok, err := s.Agents.Get(ctx, c.Agent)
	if err != nil || !ok {
		return "error: cannot read your own agent folder", true
	}
	id := instance(in.Agent, asker.Unit)
	switch {
	case !asker.MayAsk(id):
		if asker.AsksAny() {
			return "not allowed: you do not ask yourself", true
		}
		return fmt.Sprintf("not allowed: you may ask only %s (agents in your agent.yaml)", strings.Join(asker.Asks, ", ")), true
	case inChain(c.Run.Chain, agents.NameOf(id)):
		return fmt.Sprintf("not allowed: %s is already part of this run (%s); it cannot be asked again", agents.NameOf(id), strings.Join(c.Run.Chain, " → ")), true
	case len(c.Run.Chain) >= MaxChain:
		return fmt.Sprintf("not allowed: this run already holds %d agents (%s), the most there may be; do it yourself or answer with what you have", len(c.Run.Chain), strings.Join(c.Run.Chain, " → ")), true
	}
	target, ok, err := s.Agents.Get(ctx, id)
	if err != nil {
		return "error: " + err.Error(), true
	}
	if !ok {
		return "error: there is no agent " + id, true
	}
	who := c.Person
	if why, err := s.mayAnswer(ctx, c, target); err != nil {
		return "error: " + err.Error(), true
	} else if why != "" {
		log.Printf("agents: %s: %s may not ask %s: %s", who, c.Agent, target.ID(), why)
		return "not allowed: " + why, true
	}
	n := c.Run.Asks.Add(target.ID())
	if n > MaxAsks {
		return fmt.Sprintf("not allowed: you asked %s %d times this turn already, the most there may be. Answer with what you have and say plainly what is missing.", target.ID(), MaxAsks), true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks you, working for %s", c.Agent, who)
	if c.Run.Unattended {
		b.WriteString(" with nobody watching")
	}
	fmt.Fprintf(&b, ".\n\nTask: %s\n", strings.TrimSpace(in.Task))
	if strings.TrimSpace(in.Limits) != "" {
		fmt.Fprintf(&b, "Limits: %s\n", strings.TrimSpace(in.Limits))
	}
	if strings.TrimSpace(in.DoneWhen) != "" {
		fmt.Fprintf(&b, "A good answer: %s\n", strings.TrimSpace(in.DoneWhen))
	}
	fmt.Fprintf(&b, "\nYour last message is your answer; %s gets it word for word. %s", c.Agent, statusAsk)
	text := b.String()
	msg := func(bool) (string, []sandbox.Image) { return text, nil }
	run := sandbox.Run{
		Token: c.Token, StartedBy: "agent " + c.Agent, Chain: append(slices.Clone(c.Run.Chain), target.ID()),
		Unattended: c.Run.Unattended, Owner: c.Run.Owner, Admin: c.Run.Admin, Merge: c.Run.Merge, Answered: c.Run.Answered,
	}
	key := c.Chat + "\x00" + target.ID()
	log.Printf("agents: %s: %s asks %s (%s)", who, c.Agent, target.ID(), strings.Join(run.Chain, " → "))
	// The feed shows the hand-off to the asked agent's unit and to the
	// person, and how it ended.
	asked := "asked"
	if n > 1 {
		asked = fmt.Sprintf("asked again (%d. time)", n)
	}
	ev := events.Event{Source: events.SourceAgents, Kind: events.KindAsk, Status: events.StatusRunning,
		Unit: target.Unit, Audience: events.AudienceMembers, Actor: who, Agent: target.ID(), StartedBy: "agent " + c.Agent,
		Title: fmt.Sprintf("%s %s %s, for %s: %s", c.Agent, asked, target.ID(), who, strings.TrimSpace(in.Task)),
		Key:   fmt.Sprintf("ask:%s:%s:%d", c.Agent, target.ID(), time.Now().UnixNano())}
	// record ends the hand-off in the feed and says whether it was a lesson:
	// done this time, after a miss earlier this turn.
	record := func(ctx context.Context, answer string, err error) (learnt bool) {
		o := outcomeOf(answer)
		miss := sandbox.Miss{}
		if err != nil {
			miss = sandbox.Miss{Status: "failed", Why: err.Error(), Task: in.Task}
		} else if o.Status == StatusPartial || o.Status == StatusStuck {
			miss = sandbox.Miss{Status: o.Status, Why: o.Why, Task: in.Task}
		}
		before, missed := c.Run.Asks.Answered(target.ID(), miss)
		learnt = missed && err == nil && o.Status == StatusDone
		if s.Events == nil {
			return learnt
		}
		ev.Status, ev.Detail = events.StatusOK, answer
		// The feed says when it did not finish, so the lead's next ask,
		// and a person reading, see why.
		if miss.Status != "" && err == nil {
			ev.Title = strings.TrimSuffix(ev.Title, ".") + " (" + o.Status + ")"
		}
		if err != nil {
			ev.Status, ev.Detail = events.StatusFailed, strings.TrimSpace(err.Error()+"\n\n"+answer)
		}
		s.Events.Record(ctx, ev)
		if learnt {
			s.Events.Record(ctx, lesson(ev, c.Agent, target.ID(), before, in.Task))
		}
		return learnt
	}
	if s.Events != nil {
		s.Events.Record(ctx, ev)
	}

	if in.Background {
		channel, inChannel := strings.CutPrefix(c.Chat, "channel:")
		if s.Poster == nil || (!inChannel && !c.Run.Unattended) {
			return "error: background works only in a channel, where the answer can be posted; ask without it", true
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.turnTimeout())
			defer cancel()
			out := &collect{}
			_, err := s.sandboxTurn(ctx, run, c.Person, c.Chat, key, target, msg, out)
			answer := out.answer()
			// Posted on its own time: a turn that ran out of its own has
			// used ctx up, and what it got done is still to be said.
			pctx, done := context.WithTimeout(context.Background(), time.Minute)
			defer done()
			record(pctx, answer, err)
			if err != nil {
				answer = strings.TrimSpace(answer + "\n\nIt stopped: " + err.Error())
			}
			if _, err := s.Poster.Post(pctx, gateway.Post{Agent: target.ID(), Channel: channel,
				Text: fmt.Sprintf("For %s, asked by %s:\n\n%s", who, c.Agent, answer)}); err != nil {
				log.Printf("agents: %s: %s's answer for %s: %v", who, target.ID(), c.Agent, err)
			}
		}()
		where := "this channel"
		if !inChannel {
			where = "the channel of " + target.Unit
		}
		return fmt.Sprintf("%s is working on it and will post its answer in %s. Tell the person so; do not wait for it.", target.ID(), where), false
	}

	out := &collect{show: c.Run.Show, prefix: "[" + target.ID() + "] "}
	if out.show != nil {
		out.show("\n")
	}
	_, err = s.sandboxTurn(ctx, run, c.Person, c.Chat, key, target, msg, out)
	answer := out.answer()
	learnt := record(context.WithoutCancel(ctx), answer, err)
	if err != nil {
		return fmt.Sprintf("%s stopped: %v\nWhat it said before: %s", target.ID(), err, answer), true
	}
	if answer == "" {
		answer = "(no answer)"
	}
	hint := retryHint(target.ID(), outcomeOf(answer), n)
	if learnt && slices.Contains(asker.Tools, "propose_skill") {
		hint = fmt.Sprintf("\n\nAsking again with a sharper task helped. Once you have answered, propose_skill for %s (or yourself) with what the better task said, so that the next run does not hit the same gap.", agents.NameOf(target.ID()))
	}
	return target.ID() + " answers:\n" + answer + hint, false
}

// lesson is what the feed keeps of an ask that helped: asker asked target
// again, sharper, after a miss, and this time it was done. ask is the
// hand-off's own event; before the miss and task the second task.
func lesson(ask events.Event, asker, target string, before sandbox.Miss, task string) events.Event {
	l := ask
	l.Kind, l.Status, l.Key = events.KindLesson, events.StatusOK, ask.Key+":lesson"
	why := before.Status
	if before.Why != "" {
		why += ": " + before.Why
	}
	l.Title = fmt.Sprintf("%s got %s to finish by asking again sharper; before, it was %s", asker, target, why)
	l.Detail = fmt.Sprintf("First task: %s\n\n%s was %s.\n\nThe task that worked: %s", strings.TrimSpace(before.Task), target, why, strings.TrimSpace(task))
	return l
}

// instance is the agent asked for as it works: in the unit named, as
// <unit>/<agent>, or else, by its name (or garrison/<name>), in ns.
func instance(id, ns string) string {
	if pre, name, ok := strings.Cut(id, "/"); ok && pre != agents.PlatformUnit {
		return pre + "/" + name
	}
	return ns + "/" + agents.NameOf(id)
}

// mayAnswer says why target may not work for the caller's person here: one
// they may not use, or a private agent where outsiders read. "" if it may.
func (s *Server) mayAnswer(ctx context.Context, c sandbox.Caller, target agents.Agent) (string, error) {
	if s.Gitea != nil {
		person := gateway.Person{Name: c.Person, Admin: c.Run.Admin, Unattended: c.Run.Unattended, Owner: c.Run.Owner}
		if c.Run.Unattended {
			// Nobody is watching: the owner must be one who may use it.
			person = gateway.Person{Name: c.Run.Owner}
		}
		if ok, err := s.Gitea.CanUse(ctx, &target, person); err != nil {
			return "", err
		} else if !ok {
			return fmt.Sprintf("%s is for the people of %s, and %s is not one of them", target.ID(), target.Unit, person.Name), nil
		}
	}
	return s.privateHere(ctx, target, c.Chat)
}

// privateHere says why a private agent may not answer in chat, where people
// outside its unit would read it: a channel other than its unit's,
// which only its people read. "" if it may: a public agent, a private chat,
// a routine.
func (s *Server) privateHere(ctx context.Context, a agents.Agent, chat string) (string, error) {
	id, inChannel := strings.CutPrefix(chat, "channel:")
	if !inChannel || s.Gitea == nil {
		return "", nil
	}
	private, err := s.Gitea.AgentPrivate(ctx, &a)
	if err != nil || !private {
		return "", err
	}
	if s.Poster != nil {
		info, err := s.Poster.Channel(ctx, id)
		if err != nil {
			return "", err
		}
		if info.Members && info.Unit == a.Unit {
			return "", nil
		}
	}
	return fmt.Sprintf("%s works on private repos of %s, so it answers only where only %s's people read: ask it in the channel %s or in a private chat", a.ID(), a.Unit, a.Unit, a.Unit), nil
}
