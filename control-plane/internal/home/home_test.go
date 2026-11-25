package home

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/events"
)

type access map[string]events.Visibility

func (a access) Visibility(_ context.Context, who string) (events.Visibility, error) {
	v := a[who]
	v.Person = who
	return v, nil
}

func server() (*Server, *events.Memory) {
	m := &events.Memory{}
	ctx := context.Background()
	m.Record(ctx, events.Event{Source: events.SourceDeployer, Kind: events.KindDeploy, Status: events.StatusOK, Unit: "todo", Repo: "app", Commit: "m1", Title: "deployed todo/todo@main"})
	m.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPush, Unit: "todo", Repo: "secret", Commit: "s1", Title: "alice pushed to secret"})
	m.Record(ctx, events.Event{Source: events.SourceGitea, Kind: events.KindPRMerged, Status: events.StatusOK, Unit: "todo", Repo: "app", PR: 3, Commit: "m1", Title: "carol merged #3 <b>Our todos</b>"})
	m.Record(ctx, events.Event{Source: events.SourceGateway, Kind: "browse", Status: events.StatusRefused, Unit: "todo", Audience: events.AudienceMembers, Actor: "bob", Agent: "todo/media", Title: "bob through todo/media: browse was refused"})
	a := access{
		"alice": {All: true},
		"dave":  {Repos: map[string]bool{"todo/app": true, "todo/config": true}},
	}
	return &Server{Events: m, Access: a, Domain: "home.test", Now: func() time.Time { return time.Now() }}, m
}

func get(s *Server, who, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if who != "" {
		r.Header.Set(UserHeader, who)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestOnlySignedIn(t *testing.T) {
	s, _ := server()
	for _, p := range []string{"/", "/feed?after=0", "/e/1"} {
		if w := get(s, "", p); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a person: %d", p, w.Code)
		}
	}
	if w := get(s, "", "/healthz"); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
}

func TestEachSeesWhatTheyMayRead(t *testing.T) {
	s, _ := server()
	all := get(s, "alice", "/").Body.String()
	for _, want := range []string{"deployed todo/todo@main", "alice pushed to secret", "browse was refused", "carol merged #3 &lt;b&gt;Our todos&lt;/b&gt;"} {
		if !strings.Contains(all, want) {
			t.Errorf("alice misses %q", want)
		}
	}
	dave := get(s, "dave", "/").Body.String()
	if !strings.Contains(dave, "deployed todo/todo@main") || strings.Contains(dave, "pushed to secret") || strings.Contains(dave, "browse was refused") {
		t.Errorf("dave's feed:\n%s", dave)
	}
	if w := get(s, "dave", "/e/2"); w.Code != http.StatusNotFound {
		t.Errorf("dave opens the private push: %d", w.Code)
	}
	// Bob is not in todo, but sees what he did there.
	if bob := get(s, "bob", "/").Body.String(); !strings.Contains(bob, "browse was refused") || strings.Contains(bob, "deployed") {
		t.Errorf("bob's feed:\n%s", bob)
	}
}

func TestFilters(t *testing.T) {
	s, _ := server()
	if body := get(s, "alice", "/?view=problems").Body.String(); !strings.Contains(body, "browse was refused") || strings.Contains(body, "deployed todo") {
		t.Errorf("problems:\n%s", body)
	}
	if body := get(s, "alice", "/?view=deploys").Body.String(); !strings.Contains(body, "deployed todo") || strings.Contains(body, "merged") {
		t.Errorf("deploys:\n%s", body)
	}
	if body := get(s, "alice", "/?q=CAROL").Body.String(); !strings.Contains(body, "merged") || strings.Contains(body, "deployed todo") {
		t.Errorf("search:\n%s", body)
	}
}

func TestLiveUpdates(t *testing.T) {
	s, m := server()
	if body := get(s, "alice", "/feed?after=4").Body.String(); strings.Contains(body, "<li") {
		t.Errorf("nothing new, but:\n%s", body)
	}
	m.Record(context.Background(), events.Event{Kind: events.KindPipeline, Unit: "todo", Repo: "app", Title: "pipeline on main: success"})
	body := get(s, "alice", "/feed?after=4").Body.String()
	if !strings.Contains(body, "pipeline on main") || strings.Contains(body, "<html") || strings.Contains(body, "deployed todo") {
		t.Errorf("new entries:\n%s", body)
	}
}

func TestTheThreadShowsHowItWasHandled(t *testing.T) {
	s, _ := server()
	body := get(s, "dave", "/e/3").Body.String()
	if !strings.Contains(body, "How it was handled") || !strings.Contains(body, "deployed todo/todo@main") {
		t.Errorf("the merge's page:\n%s", body)
	}
}

func TestAPI(t *testing.T) {
	s, _ := server()
	var es []events.Event
	if err := json.NewDecoder(get(s, "dave", "/api/events?ns=todo").Body).Decode(&es); err != nil || len(es) != 2 {
		t.Fatalf("dave's events: %v %+v", err, es)
	}
	var one struct {
		Event  events.Event   `json:"event"`
		Thread []events.Event `json:"thread"`
	}
	json.NewDecoder(get(s, "dave", "/api/events/1").Body).Decode(&one)
	if one.Event.Kind != events.KindDeploy || len(one.Thread) != 2 {
		t.Errorf("the deploy and its thread: %+v", one)
	}
	if w := get(s, "dave", "/api/events/2"); w.Code != http.StatusNotFound {
		t.Errorf("dave gets the private push: %d", w.Code)
	}
}

// The Lessons view shows what agents learnt, to whom may see each.
func TestLessonsView(t *testing.T) {
	s, m := server()
	ctx := context.Background()
	m.Record(ctx, events.Event{Source: events.SourceAgents, Kind: events.KindLesson, Status: events.StatusOK, Unit: "todo", Audience: events.AudienceMembers,
		Actor: "bob", Agent: "todo/scout", Title: "todo/coder got todo/scout to finish by asking again sharper"})
	body := get(s, "alice", "/?view=lessons").Body.String()
	if !strings.Contains(body, "got todo/scout to finish") || strings.Contains(body, "deployed todo") || !strings.Contains(body, ">Lessons<") {
		t.Errorf("the Lessons view:\n%s", body)
	}
	if body := get(s, "dave", "/?view=lessons").Body.String(); strings.Contains(body, "got todo/scout to finish") {
		t.Error("dave, not in todo, sees its lesson")
	}
}
