package home

import (
	"fmt"
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
	if who == "newcomer" {
		return events.Visibility{}, events.ErrNoAccount
	}
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
	m.Record(ctx, events.Event{Source: events.SourceGateway, Kind: "browse", Status: events.StatusRefused, Unit: "todo", Audience: events.AudienceMembers, Actor: "bob", Agent: "todo/designer", Title: "bob through todo/designer: browse was refused"})
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

// An entry's page tells what belongs to it, newest first, and what ties
// them; for the feed's window, only the entry, without the page around it.
func TestTheStoryOfAnEntry(t *testing.T) {
	s, _ := server()
	body := get(s, "dave", "/e/3").Body.String()
	list := body[strings.Index(body, "Everything about")+1:]
	merge, deploy := strings.Index(list, "carol merged #3"), strings.Index(list, "deployed todo/todo@main")
	if !strings.Contains(body, "Everything about pull request #3 of todo/app") || merge < 0 || deploy < 0 || merge > deploy { // the merge is the newer here
		t.Errorf("the merge's page:\n%s", body)
	}
	part := get(s, "dave", "/e/3?part=1").Body.String()
	if strings.Contains(part, "<html") || strings.Contains(part, "← Feed") || !strings.Contains(part, "Everything about pull request #3") {
		t.Errorf("the window's part:\n%s", part)
	}
	if !strings.Contains(body, `id="modal"`) {
		t.Error("the page has no window")
	}
}

// The same commit deployed again and again is one step, with a count.
func TestRepeatsFoldIntoOneStep(t *testing.T) {
	at := time.Now()
	e := events.Event{ID: 15, Kind: events.KindDeploy, Status: events.StatusOK, Unit: "garrison", Repo: "sso", Commit: "22bb2201234", Title: "deployed the system stack sso at 22bb220", At: at}
	again := func(id int64, d time.Duration) events.Event { x := e; x.ID, x.At = id, at.Add(d); return x }
	st := storyOf(e, []events.Event{again(90, 2*time.Hour), e, again(40, time.Hour)})
	if st.About != "commit 22bb220 of garrison/sso" || !st.Only || len(st.Steps) != 1 || st.Steps[0].Times != 3 || !st.Steps[0].This {
		t.Errorf("%+v", st)
	}
	if !st.Steps[0].At[2].Equal(at) || !st.Steps[0].At[0].Equal(at.Add(2*time.Hour)) {
		t.Errorf("not newest first: %v", st.Steps[0].At)
	}
	// A deploy of a commit names the repo its push or merge came from.
	d := events.Event{ID: 221, Kind: events.KindDeploy, Unit: "todo", Commit: "2d455f9133eb", At: at}
	push := events.Event{ID: 216, Kind: events.KindPush, Unit: "todo", Repo: "config", Commit: "2d455f9133eb", At: at.Add(-time.Minute)}
	if st := storyOf(d, []events.Event{push, d}); st.About != "commit 2d455f9 of todo/config" || st.Steps[0].E.ID != 221 {
		t.Errorf("%+v", st)
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
	if !strings.Contains(body, "@scout</span> to finish") || !strings.Contains(body, "@coder</span> got") || strings.Contains(body, "deployed todo") || !strings.Contains(body, ">Lessons<") {
		t.Errorf("the Lessons view:\n%s", body)
	}
	if body := get(s, "dave", "/?view=lessons").Body.String(); strings.Contains(body, "@scout</span> to finish") {
		t.Error("dave, not in todo, sees its lesson")
	}
}

// Someone without a Gitea account yet, as the first root admin right after
// the setup, is sent to make it, and back.
func TestTheFirstVisitMakesTheGiteaAccount(t *testing.T) {
	s, _ := server()
	w := get(s, "newcomer", "/models")
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Open Gitea once") ||
		!strings.Contains(body, `href="https://git.home.test/user/oauth2/keycloak?redirect_to=https%3A%2F%2Fhome.test%2Fmodels"`) {
		t.Errorf("%d\n%s", w.Code, body)
	}
}

// A commit links to Gitea, in the repo it is in, also when its event
// names none.
func TestCommitsLinkToGitea(t *testing.T) {
	sys := events.Event{Kind: events.KindDeploy, Commit: "22bb2201234", Title: "deployed the system stack sso at 22bb220"}
	restore := events.Event{Kind: events.KindDeploy, Unit: "todo", Commit: "2d455f9133eb", Title: "todo/config/restores/todo.yaml: todo not started again: ..."}
	app := events.Event{Kind: events.KindDeploy, Unit: "todo", Repo: "app", Commit: "m1"}
	pushed := events.Event{Kind: events.KindDeploy, Unit: "shop", Commit: "abc"}
	push := events.Event{Kind: events.KindPush, Unit: "shop", Repo: "site", Commit: "abc"}
	for _, c := range []struct {
		e       events.Event
		related []events.Event
		want    string
	}{
		{sys, nil, "garrison/sso"},
		{restore, nil, "todo/config"},
		{app, nil, "todo/app"},
		{pushed, []events.Event{push}, "shop/site"},
		{events.Event{Kind: events.KindDeploy, Unit: "shop", Commit: "zzz"}, nil, ""},
	} {
		if got := commitRepo(c.e, c.related); got != c.want {
			t.Errorf("%q: %q, want %q", c.e.Title, got, c.want)
		}
	}
	s, m := server()
	m.Record(context.Background(), sys)
	es, _ := m.List(context.Background(), events.Query{Visibility: events.Visibility{All: true}})
	body := get(s, "alice", fmt.Sprintf("/e/%d", es[0].ID)).Body.String()
	if !strings.Contains(body, `href="https://git.home.test/garrison/sso/commit/22bb2201234"`) {
		t.Errorf("no link to the commit:\n%s", body)
	}
}
