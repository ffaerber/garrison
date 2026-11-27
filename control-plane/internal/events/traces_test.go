package events

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestSeesTrace(t *testing.T) {
	private := Trace{Unit: "todo", Person: "bob", Chat: "c1"}
	channel := Trace{Unit: "todo", Person: "bob", Chat: "channel:x", Shared: true}
	platform := Trace{Unit: PlatformUnit, Person: "carol", Shared: true}
	cases := []struct {
		v    Visibility
		want [3]bool
	}{
		{Visibility{Person: "bob"}, [3]bool{true, true, false}},
		{Visibility{Person: "dave", Members: map[string]bool{"todo": true}}, [3]bool{false, true, false}},
		{Visibility{Person: "alice", All: true}, [3]bool{false, false, true}},
		{Visibility{Person: "erin", Repos: map[string]bool{"todo/app": true}}, [3]bool{false, false, false}},
	}
	for _, c := range cases {
		for i, tr := range []Trace{private, channel, platform} {
			if got := c.v.SeesTrace(tr); got != c.want[i] {
				t.Errorf("%s sees trace %d: %v", c.v.Person, i, got)
			}
		}
	}
	if !Shared("channel:x") || !Shared("routine:a/b/c") || Shared("local:1") || Shared("") {
		t.Error("Shared")
	}
	if c := clip("héllo wörld", 2); c != "h … (cut)" {
		t.Errorf("clip on a rune: %q", c)
	}
}

// TestPostgresTraces runs the traces against a real database, as
// TestPostgres: the SQL must show what SeesTrace allows, and no more.
func TestPostgresTraces(t *testing.T) {
	url := os.Getenv("GARRISON_TEST_POSTGRES")
	if url == "" {
		t.Skip("GARRISON_TEST_POSTGRES is not set")
	}
	pg, err := Open(ctx, url, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Pool.Exec(ctx, "TRUNCATE agent_traces RESTART IDENTITY"); err != nil {
		t.Fatal(err)
	}
	private := pg.Begin(ctx, Trace{Unit: "todo", Agent: "todo/coder", Person: "bob", Chat: "c1", Task: "private"})
	channel := pg.Begin(ctx, Trace{Unit: "todo", Agent: "todo/designer", Person: "bob", Chat: "channel:x", Shared: true, Task: strings.Repeat("x", MaxStepText+10)})
	platform := pg.Begin(ctx, Trace{Unit: PlatformUnit, Agent: "garrison/operator", Person: "carol", Shared: true, Task: "platform"})
	pg.Add(ctx, channel, Step{Kind: StepThinking, Text: "hmm"}, Step{Kind: StepTool, Tool: "generate_media", Text: `{"a":1}`})
	pg.Add(ctx, channel, Step{Kind: StepResult, Tool: "generate_media", Text: strings.Repeat("y", MaxStepText*2), Error: true})
	pg.End(ctx, channel, StatusOK, 1)

	ids := func(v Visibility) []int64 {
		list, err := pg.Traces(ctx, TraceQuery{Visibility: v})
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, tr := range list {
			out = append(out, tr.ID)
		}
		return out
	}
	eq := func(a []int64, b ...int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if got := ids(Visibility{Person: "bob"}); !eq(got, channel, private) {
		t.Errorf("bob: %v", got)
	}
	if got := ids(Visibility{Person: "dave", Members: map[string]bool{"todo": true}}); !eq(got, channel) {
		t.Errorf("dave: %v", got)
	}
	if got := ids(Visibility{Person: "alice", All: true}); !eq(got, platform) {
		t.Errorf("alice: %v", got)
	}
	if got := ids(Visibility{Person: "erin"}); len(got) != 0 {
		t.Errorf("erin: %v", got)
	}
	tr, ok, err := pg.Trace(ctx, channel, Visibility{Person: "dave", Members: map[string]bool{"todo": true}})
	if err != nil || !ok || tr.Status != StatusOK || tr.Calls != 1 || len(tr.Steps) != 3 || tr.Steps[1].Tool != "generate_media" || !tr.Steps[2].Error {
		t.Fatalf("trace: %v %v %+v", err, ok, tr)
	}
	if len(tr.Task) > MaxStepText+20 || len(tr.Steps[2].Text) > MaxStepText+20 {
		t.Error("long texts are not cut")
	}
	if _, ok, _ := pg.Trace(ctx, private, Visibility{Person: "dave", Members: map[string]bool{"todo": true}}); ok {
		t.Error("dave reads bob's private chat")
	}
	pg.Pool.Exec(ctx, "UPDATE agent_traces SET at = now() - interval '15 days' WHERE id = $1", private)
	pg.PruneTraces(ctx)
	if got := ids(Visibility{Person: "bob"}); !eq(got, channel) {
		t.Errorf("after the prune: %v", got)
	}
}
