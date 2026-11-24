package sandbox

import (
	"reflect"
	"testing"
	"time"
)

// Turns lists only the sessions with a turn, for whom they work: a
// routine's owner, else the person whose message started it.
func TestRunning(t *testing.T) {
	t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	m := &Manager{}
	m.init()
	m.sessions["a"] = &Session{want: Want{Agent: "lab/coder", Person: "alice"},
		run: &Run{StartedBy: "routine lab/coder/news-daily", Unattended: true, Owner: "alice", Chain: []string{"lab/coder"}}, since: t0.Add(time.Minute)}
	m.sessions["b"] = &Session{want: Want{Agent: "todo/operator", Person: "bob"},
		run: &Run{StartedBy: "person", Chain: []string{"todo/operator"}}, since: t0}
	m.sessions["c"] = &Session{want: Want{Agent: "todo/scout", Person: "bob"}} // between turns
	got := m.Turns()
	want := []Running{
		{Unit: "todo", Agent: "operator", For: "bob", StartedBy: "person", Chain: []string{"todo/operator"}, Since: t0},
		{Unit: "lab", Agent: "coder", For: "alice", StartedBy: "routine lab/coder/news-daily", Chain: []string{"lab/coder"}, Since: t0.Add(time.Minute)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("running:\n got %+v\nwant %+v", got, want)
	}
	if got := (&Manager{}).Turns(); got == nil || len(got) != 0 {
		t.Errorf("a manager without sessions: %#v, want an empty list", got)
	}
}
