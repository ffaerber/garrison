package agentapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
)

func TestARoutineAsksItsPersonAndGoesOn(t *testing.T) {
	s, _, _ := setup(t)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/repos/homepage/config/raw/members.yaml" {
			io.WriteString(w, "admins: [alice]\nmembers: [bob]\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer gitea.Close()
	g := &gateway.Gitea{URL: gitea.URL, User: "admin", Password: "pw"}
	s.Agents = fakeAgents{{Unit: "homepage", Name: "dev", Prompt: "Dev.", Routines: []agents.Routine{
		{Name: "colours", Every: time.Hour, Raw: "1h", Owner: "bob", Task: `call ask_person {"question":"Blue or green for the header?"}`},
	}}}
	d := withSandboxes(t, s)
	poster := &fakePoster{}
	s.Poster = poster
	feed := &events.Memory{}
	s.Events = feed
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := &events.MemoryQuestions{}
	q := &Questions{Server: s, Store: store, Now: clock}
	s.Questions = q
	r := &Routines{Server: s, Gitea: g, Now: clock}
	ctx := context.Background()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("waited in vain for %s; posts: %+v", what, poster.all())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waiting := func() []events.Question {
		list, _ := store.Questions(ctx, events.QuestionWaiting)
		return list
	}

	r.Check(ctx) // first seen
	now = now.Add(61 * time.Minute)
	r.Check(ctx)
	waitFor("the question", func() bool { return len(waiting()) == 1 })
	waitFor("the routine to end", func() bool {
		es, _ := feed.List(ctx, events.Query{Visibility: events.Visibility{All: true}, Kinds: []string{events.KindRoutine}, Status: events.StatusOK})
		return len(es) == 1
	})

	// Posted in the unit's channel, naming bob; nothing else is, as the
	// routine waits.
	posts := poster.all()
	if len(posts) != 1 || posts[0].Mention != "bob" || posts[0].Channel != "" || !strings.Contains(posts[0].Text, "Blue or green for the header?") ||
		!strings.Contains(posts[0].Text, "Reply in this thread") {
		t.Fatalf("posts: %+v", posts)
	}
	x := waiting()[0]
	if x.Person != "bob" || x.Channel != "ch-homepage" || x.Message != "m1" || x.Root != "m1" || x.User != "u-bob" || x.Key != "routine\x00homepage/dev/colours" {
		t.Errorf("question: %+v", x)
	}
	// bob's dashboard lists it.
	mine, _ := feed.List(ctx, events.Query{Visibility: events.Visibility{Person: "bob", Members: map[string]bool{"homepage": true}},
		Kinds: []string{events.KindQuestion}, Actor: "bob", Status: events.StatusWaiting})
	if len(mine) != 1 || !strings.Contains(mine[0].Title, "homepage/dev asks bob: Blue or green") || mine[0].Link != "https://chat.example.com/channels/ch-homepage?thread=m1" {
		t.Errorf("waiting for bob: %+v", mine)
	}

	// Someone else's reply is not the answer.
	poster.reply("ch-homepage", "m1", "u-alice", "Red!")
	q.Check(ctx)
	if len(waiting()) != 1 {
		t.Fatal("alice answered for bob")
	}

	// bob's is: the run goes on in the same sandbox, and says so in the
	// thread.
	poster.reply("ch-homepage", "m1", "u-bob", "Green, please.")
	q.Check(ctx)
	waitFor("the answer in the thread", func() bool { return len(poster.all()) == 2 })
	q.wg.Wait()
	reply := poster.all()[1]
	if reply.Parent != "m1" || reply.Channel != "ch-homepage" || !strings.Contains(reply.Text, "Their answer: Green, please.") {
		t.Errorf("reply: %+v", reply)
	}
	if d.Created != 1 {
		t.Errorf("%d sandboxes; the run goes on in its own", d.Created)
	}
	if len(waiting()) != 0 {
		t.Error("still waiting")
	}
	if es, _ := feed.List(ctx, events.Query{Visibility: events.Visibility{All: true}, Kinds: []string{events.KindQuestion}}); len(es) != 1 || es[0].Status != events.StatusOK || !strings.Contains(es[0].Detail, "bob answered: Green, please.") {
		t.Errorf("the question in the feed: %+v", es)
	}

	// Asked again and left unanswered: after a day, it stops waiting.
	now = now.Add(time.Hour)
	r.Check(ctx)
	waitFor("the second question", func() bool { return len(waiting()) == 1 })
	now = now.Add(QuestionWait + time.Minute)
	q.Check(ctx)
	if len(waiting()) != 0 {
		t.Fatal("waits forever")
	}
	last := poster.all()[len(poster.all())-1]
	if last.Parent != "m3" || !strings.Contains(last.Text, "No answer within 24 hours") {
		t.Errorf("expired: %+v", last)
	}
}

// In a chat, the person reads the answer: the agent asks there.
func TestAskPersonOnlyWithNobodyWatching(t *testing.T) {
	s, _, _ := setup(t)
	withSandboxes(t, s)
	s.Poster = &fakePoster{}
	s.Questions = &Questions{Server: s, Store: &events.MemoryQuestions{}}
	_, content := chatIn(t, s, "chat-1", "homepage/reader", Message{Role: "user", Content: `call ask_person {"question":"Which?"}`})
	if !strings.Contains(content, "not needed: a person reads your answer") {
		t.Errorf("content: %q", content)
	}
	if list, _ := s.Questions.Store.Questions(context.Background(), events.QuestionWaiting); len(list) != 0 {
		t.Errorf("kept: %+v", list)
	}
}
