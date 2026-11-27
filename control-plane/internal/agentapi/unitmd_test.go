package agentapi

import (
	"context"
	"strings"
	"testing"

	"garrison/control-plane/internal/agents"
)

type fakeUnits struct {
	md    map[string]string // unit -> unit.md
	asked []string
}

func (f *fakeUnits) UnitMD(_ context.Context, unit, person string) (string, error) {
	f.asked = append(f.asked, "unit "+unit+" for "+person)
	return f.md[unit], nil
}
func (f *fakeUnits) PersonUnitsMD(_ context.Context, person string) (string, error) {
	f.asked = append(f.asked, "units of "+person)
	return f.md["todo"], nil
}

// A turn knows where it works: the unit's unit.md in a channel, the
// person's units in a private chat; in full once, then only when it changed.
func TestEachTurnKnowsItsUnit(t *testing.T) {
	f := &fakeUnits{md: map[string]string{"todo": "# todo\n\n## Apps\n\n- todo: running\n"}}
	s := &Server{Units: f}
	ctx := context.Background()
	coder := agents.Agent{Unit: "todo", Name: "coder"}
	commander := agents.Agent{Unit: agents.PlatformUnit, Name: "commander"}

	got := s.unitContext(ctx, "channel:abc", coder, "bob", false)
	if !strings.Contains(got, "The unit you work in, todo") || !strings.Contains(got, "- todo: running") {
		t.Errorf("in todo's channel:\n%s", got)
	}
	if got := s.unitContext(ctx, "channel:abc", coder, "bob", false); !strings.Contains(got, "unchanged since your last turn") || strings.Contains(got, "## Apps") {
		t.Errorf("again, unchanged:\n%s", got)
	}
	if got := s.unitContext(ctx, "channel:abc", coder, "bob", true); !strings.Contains(got, "## Apps") {
		t.Errorf("a fresh session gets it in full:\n%s", got)
	}
	f.md["todo"] += "- dev-todo: not running\n"
	if got := s.unitContext(ctx, "channel:abc", coder, "bob", false); !strings.Contains(got, "dev-todo: not running") {
		t.Errorf("changed:\n%s", got)
	}

	got = s.unitContext(ctx, "chat-1", commander, "bob", false)
	if !strings.Contains(got, "This chat belongs to no unit. The units bob is in") || !strings.Contains(got, "# todo") {
		t.Errorf("in a private chat:\n%s", got)
	}
	if f.asked[len(f.asked)-1] != "units of bob" {
		t.Errorf("asked %v", f.asked)
	}
	if got := s.unitContext(ctx, "chat-1", commander, "", false); got != "" {
		t.Errorf("without a person: %q", got)
	}
}
