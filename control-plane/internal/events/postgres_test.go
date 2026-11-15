package events

import (
	"os"
	"testing"
	"time"
)

// TestPostgres runs the store against a real database, if
// GARRISON_TEST_POSTGRES names one (a postgres:// URL of an empty database).
func TestPostgres(t *testing.T) {
	url := os.Getenv("GARRISON_TEST_POSTGRES")
	if url == "" {
		t.Skip("GARRISON_TEST_POSTGRES is not set")
	}
	pg, err := Open(ctx, url, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	app := func(e Event) Event { e.Namespace, e.Repo = "todo", "app"; return e }
	pg.Record(ctx, Event{Kind: KindRoutine, Namespace: "todo", Audience: AudienceMembers, Actor: "bob", StartedBy: "routine todo/dev/n", Status: StatusRunning, Title: "runs", Key: "r"})
	pg.Record(ctx, app(Event{Kind: "propose_patch", StartedBy: "routine todo/dev/n", PR: 7, Title: "proposes"}))
	pg.Record(ctx, app(Event{Kind: KindPRMerged, PR: 7, Commit: "m", Title: "merged"}))
	pg.Record(ctx, app(Event{Kind: KindDeploy, Commit: "m", Title: "deployed", Status: StatusOK}))
	pg.Record(ctx, Event{Kind: KindPush, Namespace: "todo", Repo: "secret", Title: "private", Key: "p"})
	pg.Record(ctx, Event{Kind: KindPush, Namespace: "todo", Repo: "secret", Title: "private, again", Key: "p"})
	pg.Record(ctx, Event{Kind: KindRoutine, Status: StatusOK, Detail: "done", Key: "r"})

	all := Visibility{All: true}
	es, err := pg.List(ctx, Query{Visibility: all})
	if err != nil || len(es) != 5 {
		t.Fatalf("all: %v %+v", err, es)
	}
	if es[0].Title != "private, again" && es[0].Title != "private" {
		t.Errorf("newest first: %+v", es[0])
	}
	r, ok, _ := pg.Get(ctx, 1, all)
	if !ok || r.Status != StatusOK || r.Detail != "done" || r.Title != "runs" {
		t.Errorf("the routine's run after its end: %+v", r)
	}
	outsider := Visibility{Person: "dave", Repos: map[string]bool{"todo/app": true, "todo/config": true}}
	es, _ = pg.List(ctx, Query{Visibility: outsider})
	if len(es) != 3 {
		t.Errorf("an outsider sees %+v", es)
	}
	es, _ = pg.List(ctx, Query{Visibility: Visibility{Person: "bob"}})
	if len(es) != 1 || es[0].Kind != KindRoutine {
		t.Errorf("bob, outside, sees what he did: %+v", es)
	}
	thread, err := pg.Related(ctx, r, all)
	if err != nil || len(thread) != 3 {
		t.Errorf("the routine's thread: %v %+v", err, thread)
	}
	es, _ = pg.List(ctx, Query{Visibility: all, Kinds: []string{KindDeploy}, Text: "DEPLOY"})
	if len(es) != 1 {
		t.Errorf("filtered: %+v", es)
	}
	es, _ = pg.List(ctx, Query{Visibility: all, After: 3})
	if len(es) != 2 {
		t.Errorf("after 3: %+v", es)
	}
	ns, _ := pg.Namespaces(ctx, outsider)
	if len(ns) != 1 || ns[0] != "todo" {
		t.Errorf("namespaces: %v", ns)
	}

	pg.Pool.Exec(ctx, "TRUNCATE routine_state")
	at := time.Date(2026, 11, 14, 6, 30, 0, 0, time.UTC)
	if err := pg.SaveRoutineState(ctx, "todo/dev/morning", RoutineState{Last: at}); err != nil {
		t.Fatal(err)
	}
	pg.SaveRoutineState(ctx, "todo/dev/on-main", RoutineState{Head: "c1"})
	pg.SaveRoutineState(ctx, "todo/dev/on-main", RoutineState{Head: "c2"})
	pg.SaveRoutineState(ctx, "todo/dev/on-alert#alert:f1:1", RoutineState{Last: at})
	if err := pg.DeleteRoutineState(ctx, "todo/dev/on-alert#alert:f1:1"); err != nil {
		t.Fatal(err)
	}
	st, err := pg.RoutineStates(ctx)
	if err != nil || len(st) != 2 || !st["todo/dev/morning"].Last.Equal(at) || st["todo/dev/on-main"].Head != "c2" || !st["todo/dev/on-main"].Last.IsZero() {
		t.Errorf("routine state: %v %+v", err, st)
	}
}
