package events

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func TestFromGitea(t *testing.T) {
	for _, c := range []struct {
		event, body string
		kind, title string
		status      string
		commit      string
		pr          int
	}{
		{"push", `{"ref":"refs/heads/main","after":"abc123","compare_url":"https://git.x/todo/app/compare/a...b","repository":{"full_name":"todo/app"},"pusher":{"login":"bob"},"sender":{"login":"bob"},"commits":[{"message":"Heading\n\nbody"}],"total_commits":1}`,
			KindPush, "bob pushed 1 commit to main", StatusInfo, "abc123", 0},
		{"push", `{"ref":"refs/tags/v1","after":"abc","repository":{"full_name":"todo/app"}}`, "", "", "", "", 0},
		{"pull_request", `{"action":"opened","repository":{"full_name":"todo/app"},"sender":{"login":"bob"},"pull_request":{"number":4,"title":"Our todos","html_url":"https://git.x/todo/app/pulls/4","user":{"login":"bob"},"head":{"ref":"change/bob-1","sha":"h1"},"base":{"ref":"main"}}}`,
			KindPROpened, "bob proposed #4 Our todos (change/bob-1 → main)", StatusInfo, "h1", 4},
		{"pull_request", `{"action":"closed","repository":{"full_name":"todo/app"},"sender":{"login":"carol"},"pull_request":{"number":4,"title":"Our todos","merged":true,"merge_commit_sha":"m1","head":{"sha":"h1"},"base":{"ref":"main"}}}`,
			KindPRMerged, "carol merged #4 Our todos into main", StatusOK, "m1", 4},
		{"pull_request_review_approved", `{"action":"reviewed","repository":{"full_name":"todo/config"},"sender":{"login":"carol"},"pull_request":{"number":2,"title":"Add bob","head":{"sha":"h2"}}}`,
			KindApproved, "carol approved #2 Add bob", StatusOK, "h2", 2},
		{"workflow_run", `{"action":"completed","repository":{"full_name":"todo/app"},"workflow_run":{"id":9,"name":"pipeline","head_branch":"main","head_sha":"m1","conclusion":"failure"}}`,
			KindPipeline, "pipeline on main: failure (pipeline)", StatusFailed, "m1", 0},
		{"workflow_run", `{"action":"in_progress","repository":{"full_name":"todo/app"},"workflow_run":{"id":9}}`, "", "", "", "", 0},
		{"status", `{"sha":"h2","context":"homecloud/approval","state":"failure","description":"needs an admin","repository":{"full_name":"todo/config"}}`,
			KindApprovalCheck, "approval check: needs an admin", StatusRefused, "h2", 0},
		{"status", `{"sha":"h2","context":"ci","state":"success","repository":{"full_name":"todo/config"}}`, "", "", "", "", 0},
		{"repository", `{"action":"created","repository":{"full_name":"todo/secret"},"sender":{"login":"alice"}}`,
			KindRepoCreated, "alice created the repo todo/secret", StatusInfo, "", 0},
	} {
		es := FromGitea(c.event, "d1", []byte(c.body))
		if c.kind == "" {
			if len(es) != 0 {
				t.Errorf("%s: %+v, want none", c.event, es)
			}
			continue
		}
		if len(es) != 1 {
			t.Fatalf("%s: %d events", c.event, len(es))
		}
		e := es[0]
		if e.Kind != c.kind || e.Title != c.title || e.Status != c.status || e.Commit != c.commit || e.PR != c.pr || e.Namespace == "" {
			t.Errorf("%s: %+v", c.event, e)
		}
	}
}

func TestKeyUpdatesTheRecord(t *testing.T) {
	m := &Memory{}
	m.Record(ctx, Event{Kind: KindRoutine, Status: StatusRunning, Title: "nightly runs", Key: "r1"})
	m.Record(ctx, Event{Kind: KindRoutine, Status: StatusOK, Detail: "all fine", Key: "r1"})
	all := m.All()
	if len(all) != 1 || all[0].Status != StatusOK || all[0].Title != "nightly runs" || all[0].Detail != "all fine" {
		t.Errorf("%+v", all)
	}
}

func TestVisibility(t *testing.T) {
	m := &Memory{}
	m.Record(ctx, Event{Kind: KindDeploy, Title: "system"})
	m.Record(ctx, Event{Kind: KindPush, Namespace: "todo", Repo: "app", Title: "public"})
	m.Record(ctx, Event{Kind: KindPush, Namespace: "todo", Repo: "secret", Title: "private"})
	m.Record(ctx, Event{Kind: KindRoutine, Namespace: "todo", Audience: AudienceMembers, Title: "routine"})
	m.Record(ctx, Event{Kind: KindDeploy, Namespace: "todo", Title: "config"})
	titles := func(v Visibility) string {
		es, _ := m.List(ctx, Query{Visibility: v})
		var out []string
		for _, e := range es {
			out = append(out, e.Title)
		}
		return strings.Join(out, ",")
	}
	outsider := Visibility{Repos: map[string]bool{"todo/app": true, "todo/config": true}}
	member := Visibility{Repos: map[string]bool{"todo/app": true, "todo/config": true, "todo/secret": true}, Members: map[string]bool{"todo": true}}
	if got := titles(outsider); got != "config,public,system" {
		t.Errorf("outsider sees %s", got)
	}
	if got := titles(member); got != "config,routine,private,public,system" {
		t.Errorf("member sees %s", got)
	}
	if got := titles(Visibility{All: true}); got != "config,routine,private,public,system" {
		t.Errorf("root admin sees %s", got)
	}
}

func TestRelatedFollowsThePullRequestToItsDeploy(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	m := &Memory{Now: func() time.Time { now = now.Add(time.Second); return now }}
	app := func(e Event) Event { e.Namespace, e.Repo = "todo", "app"; return e }
	m.Record(ctx, Event{Kind: KindRoutine, Namespace: "todo", Audience: AudienceMembers, StartedBy: "routine todo/developer/nightly", Status: StatusRunning, Key: "r"}) // 1
	m.Record(ctx, app(Event{Kind: "propose_patch", StartedBy: "routine todo/developer/nightly", PR: 7}))                                                                // 2
	m.Record(ctx, app(Event{Kind: KindPROpened, PR: 7, Commit: "h"}))                                                                                                   // 3
	m.Record(ctx, app(Event{Kind: KindPRMerged, PR: 7, Commit: "m"}))                                                                                                   // 4
	m.Record(ctx, app(Event{Kind: KindPipeline, Commit: "m"}))                                                                                                          // 5
	m.Record(ctx, app(Event{Kind: KindDeploy, Commit: "m"}))                                                                                                            // 6
	m.Record(ctx, app(Event{Kind: KindPush, Commit: "other"}))                                                                                                          // 7
	m.Record(ctx, Event{Kind: KindRoutine, Status: StatusOK, Key: "r"})
	all := Visibility{All: true}
	merged, _, _ := m.Get(ctx, 4, all)
	got, _ := m.Related(ctx, merged, all)
	if ids := idsOf(got); ids != "2 3 4 5 6" {
		t.Errorf("the merge's thread: %s", ids)
	}
	run, _, _ := m.Get(ctx, 1, all)
	got, _ = m.Related(ctx, run, all)
	if ids := idsOf(got); ids != "1 2 3 4" {
		t.Errorf("the routine's thread: %s", ids)
	}
}

func idsOf(es []Event) string {
	var s []string
	for _, e := range es {
		s = append(s, strconv.FormatInt(e.ID, 10))
	}
	return strings.Join(s, " ")
}

func TestCleanTrims(t *testing.T) {
	e := Event{Title: "a\n  b " + strings.Repeat("é", 400)}.clean(time.Now())
	if strings.Contains(e.Title, "\n") || len(e.Title) > maxTitle+len("…") || e.Status != StatusInfo {
		t.Errorf("%q", e.Title)
	}
}
