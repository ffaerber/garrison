package agentapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/changes"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/gateway"
)

type fakeCards map[string]changes.Card // ns/repo#n

func (f fakeCards) Card(_ context.Context, person, ns, repo string, n int) (changes.Card, error) {
	return f[ns+"/"+repo+"#"+itoa(n)], nil
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestAgentsFollowUpTheirChanges(t *testing.T) {
	ctx := context.Background()
	feed := &events.Memory{}
	poster := &fakePoster{}
	now := time.Now()
	cards := fakeCards{
		"lab/config#3": {Unit: "lab", Repo: "config", Number: 3, Title: "Deploy example", URL: "https://git/lab/config/pulls/3",
			Body:  "Please.\n\nAsked by alice through lab/operator.",
			Files: []changes.File{{Path: "stacks/example.yaml", Status: "added"}}},
		"lab/example#4": {Unit: "lab", Repo: "example", Number: 4, Title: "Green background", Base: "dev",
			Body: "Asked by alice through lab/coder."},
		"todo/config#5": {Unit: "todo", Repo: "config", Number: 5, Title: "Shots at 9",
			Body:  "Asked by bob (not in todo) through todo/operator.",
			Files: []changes.File{{Path: "routines.yaml", Status: "modified"}, {Path: "grants.yaml", Status: "modified"}}},
		"todo/app#6": {Unit: "todo", Repo: "app", Number: 6, Title: "By hand", Base: "main", Body: "No agent wrote this."},
	}
	f := &Followups{Feed: feed, Cards: cards, Poster: poster, Now: func() time.Time { return now }}

	// What was merged before it started is not news.
	feed.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPRMerged, Status: events.StatusOK, Unit: "lab", Repo: "config", PR: 3, Actor: "bob", Key: "old"})
	f.Check(ctx)
	f.Check(ctx)
	if len(poster.all()) != 0 {
		t.Fatalf("it said %+v of what was before", poster.all())
	}

	merge := func(ns, repo string, n int, actor, commit string) {
		feed.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPRMerged, Status: events.StatusOK,
			Unit: ns, Repo: repo, PR: n, Actor: actor, Commit: commit, Key: ns + repo + itoa(n)})
	}
	merge("lab", "config", 3, "bob", "c3")
	merge("lab", "example", 4, "alice", "m4")
	merge("todo", "config", 5, "carol", "c5")
	merge("todo", "app", 6, "carol", "m6")
	f.Check(ctx)
	posts := poster.all()
	if len(posts) != 3 {
		t.Fatalf("posts: %+v", posts)
	}
	for i, want := range []struct{ agent, person, text string }{
		{"lab/operator", "alice", "Your change [Deploy example](https://git/lab/config/pulls/3) (lab/config#3) is merged, by bob. I will say here when it is deployed."},
		{"lab/coder", "alice", "is merged, by alice. Its pipeline builds it now"},
		{"todo/operator", "bob", "Its routines run from now on, as routines.yaml says; what they find is posted here. The agents have the rights it gives from now on."},
	} {
		if p := posts[i]; p.Agent != want.agent || p.Mention != want.person || !strings.Contains(p.Text, want.text) {
			t.Errorf("post %d: %+v, want %+v", i, p, want)
		}
	}

	// The deploys: of the config's stack, by its title; of the app, by its commit; one failed.
	deploy := func(ns, repo, commit, title, status, link string) {
		feed.Record(ctx, events.Event{Source: events.SourceDeployer, Kind: events.KindDeploy, Status: status,
			Unit: ns, Repo: repo, Commit: commit, Title: title, Link: link, Detail: "image missing\nmore", Key: title + status})
	}
	deploy("lab", "example", "x1", "deployed lab/example@main at x1 as example", events.StatusOK, "https://example.home.test")
	deploy("lab", "example", "x1", "deployed lab/example@main at x1 as example", events.StatusOK, "https://example.home.test") // said once
	deploy("lab", "example", "m4", "redeployed lab/example@dev at m4 as dev-example", events.StatusFailed, "")
	deploy("lab", "other", "m4", "deployed lab/other@main at m4 as other", events.StatusOK, "")
	deploy("lab", "example", "x1", "deployed lab/example@dev at x1 as dev-example", events.StatusWaiting, "")
	f.Check(ctx)
	posts = poster.all()[3:]
	if len(posts) != 3 {
		t.Fatalf("deploy posts: %+v", posts)
	}
	if p := posts[0]; p.Agent != "lab/operator" || p.Mention != "alice" || p.Text != "Your change Deploy example (lab/config#3) is live: https://example.home.test." {
		t.Errorf("live: %+v", p)
	}
	// The failed dev deploy is the coder's change, and also an environment
	// of the operator's stack: each says it, once.
	failed := map[string]bool{}
	for _, p := range posts[1:] {
		if !strings.Contains(p.Text, "deploying it failed: redeployed lab/example@dev") || !strings.Contains(p.Text, "\n\nimage missing\n\nAsk me") {
			t.Errorf("failed: %+v", p)
		}
		failed[p.Agent] = true
	}
	if !failed["lab/coder"] || !failed["lab/operator"] {
		t.Errorf("failed deploys said by %v", failed)
	}

	// After a while it stops waiting.
	now = now.Add(followupWait + time.Minute)
	f.Check(ctx)
	if len(f.waiting) != 0 {
		t.Errorf("still waiting for %d", len(f.waiting))
	}
}

func TestAnAgentsOwnMergeIsNotRepeated(t *testing.T) {
	ctx := context.Background()
	feed := &events.Memory{}
	poster := &fakePoster{}
	cards := fakeCards{"todo/app#7": {Unit: "todo", Repo: "app", Number: 7, Title: "Fix", Base: "dev", Body: "Asked by bob through todo/developer."}}
	f := &Followups{Feed: feed, Cards: cards, Poster: poster}
	f.Check(ctx)
	feed.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPRMerged, Status: events.StatusOK, Unit: "todo", Repo: "app", PR: 7, Actor: "agent-todo-developer", Commit: "m7", Key: "m7"})
	feed.Record(ctx, events.Event{Source: events.SourceDeployer, Kind: events.KindDeploy, Status: events.StatusOK, Unit: "todo", Repo: "app", Commit: "m7", Title: "deployed todo/todo@dev at m7 as dev-todo", Link: "https://dev-todo.x", Key: "d7"})
	f.Check(ctx)
	if p := poster.all(); len(p) != 1 || !strings.Contains(p[0].Text, "is live: https://dev-todo.x") {
		t.Errorf("posts: %+v", p)
	}
}

// failingPoster fails its first posts, as before a channel is made.
type failingPoster struct {
	*fakePoster
	fail int
}

func (f *failingPoster) Post(ctx context.Context, p gateway.Post) (gateway.Posted, error) {
	if f.fail > 0 {
		f.fail--
		return gateway.Posted{}, errors.New("no channel")
	}
	return f.fakePoster.Post(ctx, p)
}

func TestANewUnitIsWelcomed(t *testing.T) {
	ctx := context.Background()
	feed := &events.Memory{}
	poster := &failingPoster{fakePoster: &fakePoster{}, fail: 2}
	now := time.Now()
	f := &Followups{Feed: feed, Cards: fakeCards{}, Poster: poster, Dashboard: "https://home.test", Now: func() time.Time { return now }}
	f.Check(ctx)
	feed.Record(ctx, events.Event{Source: events.SourceGateway, Kind: events.KindUnit, Status: events.StatusOK, Unit: "lab", Actor: "alice", Key: "ns-lab"})
	feed.Record(ctx, events.Event{Source: events.SourceGateway, Kind: events.KindUnit, Status: events.StatusOK, Unit: "gone", Actor: "bob", Key: "ns-gone"})
	f.Check(ctx) // no channels yet
	if len(poster.all()) != 0 || len(f.welcomes) != 2 {
		t.Fatalf("posts %+v, waiting %d", poster.all(), len(f.welcomes))
	}
	f.Check(ctx)
	p := poster.all()
	if len(p) != 2 || p[0].Agent != "lab/operator" || p[0].Mention != "alice" || !strings.Contains(p[0].Text, "Welcome to lab") ||
		!strings.Contains(p[0].Text, "on your dashboard, https://home.test, under Waiting for you") || len(f.welcomes) != 0 {
		t.Fatalf("posts %+v", p)
	}
	f.Check(ctx)
	if len(poster.all()) != 2 {
		t.Error("welcomed twice")
	}

	// A channel that never comes: it stops trying.
	poster.fail = 1000
	feed.Record(ctx, events.Event{Source: events.SourceGateway, Kind: events.KindUnit, Status: events.StatusOK, Unit: "x", Actor: "bob", Key: "ns-x"})
	f.Check(ctx)
	now = now.Add(welcomeWait + time.Minute)
	f.Check(ctx)
	if len(f.welcomes) != 0 {
		t.Error("still waiting to welcome")
	}
}

// A deploy read while it still runs is updated in place when it ends, under
// the same ID: it is read again, and its end said.
func TestADeployReadWhileRunningIsSaidOnceItEnds(t *testing.T) {
	ctx := context.Background()
	feed := &events.Memory{}
	poster := &fakePoster{}
	cards := fakeCards{"todo/app#4": {Unit: "todo", Repo: "app", Number: 4, Title: "White dev", Base: "dev",
		Body: "Asked by alice through todo/coder."}}
	f := &Followups{Feed: feed, Cards: cards, Poster: poster}
	f.Check(ctx)
	feed.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPRMerged, Status: events.StatusOK,
		Unit: "todo", Repo: "app", PR: 4, Actor: "alice", Commit: "c551", Key: "m4"})
	deploy := func(status string) {
		feed.Record(ctx, events.Event{Source: events.SourceDeployer, Kind: events.KindDeploy, Status: status, Unit: "todo",
			Repo: "app", Commit: "c551", Title: "redeployed todo/todo@dev at c551 as dev-todo", Link: "https://dev-todo.x", Key: "deploy:todo/todo@dev@c551"})
	}
	deploy(events.StatusRunning)
	f.Check(ctx)
	if n := len(poster.all()); n != 1 {
		t.Fatalf("after the merge and a running deploy: %+v", poster.all())
	}
	deploy(events.StatusOK)
	f.Check(ctx)
	f.Check(ctx)
	posts := poster.all()
	if len(posts) != 2 || posts[1].Text != "Your change White dev (todo/app#4) is live: https://dev-todo.x." || posts[1].Mention != "alice" {
		t.Errorf("posts: %+v", posts)
	}
}
