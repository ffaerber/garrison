package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
	"garrison/control-plane/internal/sandbox"
)

// QuestionWait is how long a question waits for its answer.
const QuestionWait = 24 * time.Hour

// maxQuestion is how long a question may be.
const maxQuestion = 4000

// askPersonSpec is the tool ask_person, for a routine that needs something
// only a person can tell it.
var askPersonSpec = toolSpec{
	Name: "ask_person",
	Description: "Asks the person this routine works for a question and waits for the answer: it is posted in the unit's channel, naming them, and shows on their dashboard. " +
		"Use it only for what you cannot find out or decide yourself: a choice, a missing fact, an approval. " +
		"After calling it, end your turn at once with one line on what you asked; you go on, in this workspace, when they answer, for up to 24 hours.",
	InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "the question, with what they need to answer it: what you are doing, the options, what you would do"},
		},
		"required": []string{"question"},
	},
}

// Questions keeps the questions agents asked people, watches their
// threads for the answers, and goes on with each run when one comes.
type Questions struct {
	Server *Server
	Store  events.QuestionStore
	Wait   time.Duration // how long a question waits; 0: QuestionWait
	Tick   time.Duration // how often to look for answers; 0: 5 seconds
	Now    func() time.Time

	mu   sync.Mutex
	busy map[int64]bool
	wg   sync.WaitGroup
}

func (q *Questions) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func (q *Questions) wait() time.Duration {
	if q.Wait > 0 {
		return q.Wait
	}
	return QuestionWait
}

// runRecord is what is kept of a run to go on with it.
type runRecord struct {
	StartedBy  string   `json:"started_by"`
	Chain      []string `json:"chain"`
	Unattended bool     `json:"unattended"`
	Owner      string   `json:"owner"`
	Merge      string   `json:"merge,omitempty"`
}

// mayAskPerson tells whether the run may ask its person: a routine's own
// agent, with nobody watching. An agent another one asked answers that one;
// in a chat the person reads the answer, where the agent asks.
func (s *Server) mayAskPerson(c sandbox.Caller) bool {
	return s.Questions != nil && s.Poster != nil && c.Run.Unattended && c.Run.Owner != "" && len(c.Run.Chain) <= 1
}

// askPerson is the tool ask_person.
func (s *Server) askPerson(ctx context.Context, c sandbox.Caller, raw json.RawMessage) (string, bool) {
	var in struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Question) == "" {
		return "error: ask_person needs a question", true
	}
	question := strings.TrimSpace(in.Question)
	switch {
	case !s.mayAskPerson(c) && !c.Run.Unattended:
		return "not needed: a person reads your answer; ask your question there", true
	case !s.mayAskPerson(c) && len(c.Run.Chain) > 1:
		return fmt.Sprintf("not allowed: %s asked you; put your question in your answer to it", c.Run.Chain[len(c.Run.Chain)-2]), true
	case !s.mayAskPerson(c):
		return "not available here", true
	case len(question) > maxQuestion:
		return fmt.Sprintf("error: a question is at most %d characters", maxQuestion), true
	case c.Run.Asked != nil && c.Run.Asked.Load():
		return "not allowed: you asked already in this turn; end it, and go on when they answer", true
	}
	ns, _, _ := strings.Cut(c.Agent, "/")
	person := c.Run.Owner
	post := gateway.Post{Agent: c.Agent, Mention: person, Text: fmt.Sprintf(
		"%s\n\n_Reply in this thread to answer; %s goes on with your answer. It waits %s._", question, c.Agent, wording(s.Questions.wait()))}
	root := ""
	if ch, msg, ok := strings.Cut(c.Run.Thread, "/"); ok {
		post.Channel, post.Parent, root = ch, msg, msg
	}
	posted, err := s.Poster.Post(ctx, post)
	switch {
	case err != nil && strings.Contains(err.Error(), gateway.ErrNoChatUser.Error()):
		return fmt.Sprintf("not possible: %s has not signed in to the chat yet, so cannot be asked there. Say in your answer what you need from them.", person), true
	case err != nil:
		return "error: " + err.Error(), true
	}
	if root == "" {
		root = posted.ID
	}
	run, _ := json.Marshal(runRecord{StartedBy: c.Run.StartedBy, Chain: c.Run.Chain, Unattended: c.Run.Unattended, Owner: c.Run.Owner, Merge: c.Run.Merge})
	q := &events.Question{
		At: s.Questions.now(), Unit: ns, Agent: c.Agent, Person: person, Text: question,
		Channel: posted.Channel, Message: posted.ID, Root: root, User: posted.User,
		Key: c.Run.Key, Chat: c.Chat, Run: run, Status: events.QuestionWaiting,
		EventKey: fmt.Sprintf("question:%s:%d", c.Agent, time.Now().UnixNano()),
	}
	if err := s.Questions.Store.AddQuestion(ctx, q); err != nil {
		return "error: the question was posted but cannot be kept, so its answer would not reach you: " + err.Error(), true
	}
	if c.Run.Asked != nil {
		c.Run.Asked.Store(true)
	}
	s.Questions.record(ctx, *q, events.StatusWaiting, "")
	log.Printf("agents: %s asks %s (question %d)", c.Agent, person, q.ID)
	return fmt.Sprintf("Asked %s in the channel of %s. End your turn now with one line on what you asked; you go on here when they answer.", person, ns), false
}

// wording is a wait in words: "24 hours".
func wording(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", int(d/time.Hour))
	}
	return d.String()
}

// record writes the question into the feed: waiting, answered or not.
func (q *Questions) record(ctx context.Context, x events.Question, status, detail string) {
	s := q.Server
	if s.Events == nil {
		return
	}
	title := x.Text
	if len(title) > 200 {
		title = title[:200] + "…"
	}
	if detail == "" {
		detail = x.Text
	}
	s.Events.Record(ctx, events.Event{Source: events.SourceAgents, Kind: events.KindQuestion, Status: status,
		Unit: x.Unit, Audience: events.AudienceMembers, Actor: x.Person, Agent: x.Agent, StartedBy: "agent " + x.Agent,
		Title:  fmt.Sprintf("%s asks %s: %s", x.Agent, x.Person, title),
		Detail: detail, Link: s.threadLink(x), Key: x.EventKey})
}

// threadLink opens the question's thread in Open WebUI, at the question.
func (s *Server) threadLink(x events.Question) string {
	root := x.Root
	if root == "" {
		root = x.Message
	}
	link := fmt.Sprintf("https://chat.%s/channels/%s", s.Domain, x.Channel)
	if root == "" {
		return link
	}
	link += "?thread=" + url.QueryEscape(root)
	if x.Message != "" && x.Message != root {
		link += "&message=" + url.QueryEscape(x.Message)
	}
	return link
}

// Run looks for answers until ctx ends.
func (q *Questions) Run(ctx context.Context) {
	tick := q.Tick
	if tick <= 0 {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		q.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check looks at each waiting question once: its answer starts the run
// again; one that waited too long ends.
func (q *Questions) Check(ctx context.Context) {
	list, err := q.Store.Questions(ctx, events.QuestionWaiting)
	if err != nil {
		log.Printf("questions: %v", err)
		return
	}
	for _, x := range list {
		q.mu.Lock()
		if q.busy == nil {
			q.busy = map[int64]bool{}
		}
		busy := q.busy[x.ID]
		q.mu.Unlock()
		if busy {
			continue
		}
		if q.now().Sub(x.At) > q.wait() {
			q.expire(ctx, x)
			continue
		}
		answer, ok, err := q.answer(ctx, x)
		if err != nil {
			log.Printf("questions: %d: %v", x.ID, err)
			continue
		}
		if !ok {
			continue
		}
		x.Status, x.Answer, x.Answered = events.QuestionAnswered, answer, q.now()
		if err := q.Store.SetQuestion(ctx, x); err != nil {
			log.Printf("questions: %d: %v", x.ID, err)
			continue
		}
		q.record(ctx, x, events.StatusOK, fmt.Sprintf("%s\n\n%s answered: %s", x.Text, x.Person, answer))
		q.mu.Lock()
		q.busy[x.ID] = true
		q.mu.Unlock()
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			defer func() {
				q.mu.Lock()
				delete(q.busy, x.ID)
				q.mu.Unlock()
			}()
			q.resume(context.WithoutCancel(ctx), x)
		}()
	}
}

// answer is the person's first reply in the question's thread after it.
func (q *Questions) answer(ctx context.Context, x events.Question) (string, bool, error) {
	thread, err := q.Server.Poster.Thread(ctx, x.Channel, x.Root)
	if err != nil {
		return "", false, err
	}
	// Replies after the question: all of the thread if it starts it.
	if i := slices.IndexFunc(thread, func(m gateway.ThreadMessage) bool { return m.ID == x.Message }); i >= 0 {
		thread = thread[i+1:]
	}
	for _, m := range thread {
		if m.User == x.User && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content), true, nil
		}
	}
	return "", false, nil
}

func (q *Questions) expire(ctx context.Context, x events.Question) {
	x.Status = events.QuestionExpired
	if err := q.Store.SetQuestion(ctx, x); err != nil {
		log.Printf("questions: %d: %v", x.ID, err)
		return
	}
	q.record(ctx, x, events.StatusFailed, fmt.Sprintf("%s\n\nNo answer within %s: %s stopped waiting.", x.Text, wording(q.wait()), x.Agent))
	if _, err := q.Server.Poster.Post(ctx, gateway.Post{Agent: x.Agent, Channel: x.Channel, Parent: x.Root,
		Text: fmt.Sprintf("No answer within %s, so I stopped waiting. Ask me again in the channel if it is still needed.", wording(q.wait()))}); err != nil {
		log.Printf("questions: %d: %v", x.ID, err)
	}
	log.Printf("questions: %d expired", x.ID)
}

// resume goes on with the run that asked x, in its sandbox, with the answer.
// What it then says is posted in the question's thread, unless it asks
// again, there.
func (q *Questions) resume(ctx context.Context, x events.Question) {
	s := q.Server
	post := func(text string) {
		if _, err := s.Poster.Post(ctx, gateway.Post{Agent: x.Agent, Channel: x.Channel, Parent: x.Root, Text: text}); err != nil {
			log.Printf("questions: %d: post: %v", x.ID, err)
		}
	}
	var rec runRecord
	if err := json.Unmarshal(x.Run, &rec); err != nil {
		log.Printf("questions: %d: %v", x.ID, err)
		return
	}
	ev := events.Event{Source: events.SourceAgents, Kind: events.KindRoutine, Status: events.StatusRunning,
		Unit: x.Unit, Audience: events.AudienceMembers, Actor: x.Person, Agent: x.Agent, StartedBy: rec.StartedBy,
		Title: fmt.Sprintf("%s goes on for %s after their answer", x.Agent, x.Person), Key: fmt.Sprintf("resume:%d", x.ID),
		Link: s.threadLink(x)}
	record := func(status, detail string) {
		if s.Events != nil {
			ev.Status, ev.Detail = status, detail
			s.Events.Record(ctx, ev)
		}
	}
	a, ok, err := s.Agents.Get(ctx, x.Agent)
	if err == nil && !ok {
		err = errors.New("the agent is gone")
	}
	if err == nil && s.Gitea != nil && rec.Owner != "" {
		// As when a routine starts: its person must still be one of the
		// unit's.
		var role string
		if role, err = s.Gitea.Role(ctx, x.Unit, rec.Owner); err == nil && role == "" {
			err = fmt.Errorf("%s is no longer in %s", rec.Owner, x.Unit)
		}
	}
	if err != nil {
		log.Printf("questions: %d: %v", x.ID, err)
		record(events.StatusFailed, err.Error())
		post("I cannot go on: " + err.Error() + ".")
		return
	}
	record(events.StatusRunning, "")
	run := sandbox.Run{StartedBy: rec.StartedBy, Chain: rec.Chain, Unattended: rec.Unattended, Owner: rec.Owner, Merge: rec.Merge, Answered: true,
		Asked: &atomic.Bool{}, Thread: x.Channel + "/" + x.Root}
	text := fmt.Sprintf(`%s answered your question.

Your question: %s

Their answer: %s

Go on with your task where you stopped. Nobody is watching, as before: your last message is posted in the question's thread, for %s and the unit's people, so say plainly what you did or found, with links. Ask again with ask_person only if you must.`, x.Person, x.Text, x.Answer, x.Person)
	msg := func(bool) (string, []sandbox.Image) { return text, nil }
	tctx, cancel := context.WithTimeout(ctx, s.turnTimeout(a))
	defer cancel()
	out := &collect{}
	_, err = s.sandboxTurn(tctx, run, rec.Owner, x.Chat, x.Key, a, msg, out)
	answer := out.answer()
	if err != nil {
		log.Printf("questions: %d: %v", x.ID, err)
		answer = strings.TrimSpace(answer + "\n\nIt stopped: " + err.Error())
		record(events.StatusFailed, answer)
	} else {
		record(events.StatusOK, answer)
	}
	if run.Asked.Load() {
		return // the new question is in the thread
	}
	if answer == "" {
		answer = "(no answer)"
	}
	post(answer)
}
