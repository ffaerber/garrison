package gateway

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"homecloud/control-plane/internal/agents"
	"homecloud/control-plane/internal/events"
)

func TestToolEvents(t *testing.T) {
	dev := &agents.Agent{Namespace: "todo", Name: "developer"}
	bob := Person{Name: "bob", StartedBy: "person"}
	args := json.RawMessage(`{"namespace":"todo","repo":"app"}`)

	if _, ok := toolEvent(bob, dev, "read_file", args, "contents", nil); ok {
		t.Error("a read went to the feed")
	}
	e, ok := toolEvent(bob, dev, "propose_patch", args, "Proposed: https://git.home.test/todo/app/pulls/5 (branch change/bob-1)", nil)
	if !ok || e.Status != events.StatusOK || e.PR != 5 || e.Link != "https://git.home.test/todo/app/pulls/5" ||
		e.Agent != "todo/developer" || e.Actor != "bob" || e.Audience != events.AudienceMembers || e.Title != "bob through todo/developer: propose patch on todo/app #5" {
		t.Errorf("propose: %+v", e)
	}
	e, ok = toolEvent(bob, dev, "browse", json.RawMessage(`{"url":"https://example.org/"}`), "", refused{"only the installation's own pages"})
	if !ok || e.Status != events.StatusRefused || e.Namespace != "todo" || !strings.Contains(e.Title, "browse was refused") || !strings.Contains(e.Title, "example.org") {
		t.Errorf("refusal: %+v", e)
	}
	e, _ = toolEvent(bob, dev, "merge_change", json.RawMessage(`{"namespace":"todo","repo":"app","number":5}`), "", errors.New("Gitea is down"))
	if e.Status != events.StatusFailed || e.PR != 5 {
		t.Errorf("failure: %+v", e)
	}
	routine := Person{Name: "agent-todo-developer", Unattended: true, Owner: "bob", StartedBy: "routine todo/developer/nightly"}
	e, _ = toolEvent(routine, dev, "propose_patch", args, "https://git.home.test/todo/app/pulls/6", nil)
	if e.Actor != "bob" || e.StartedBy != "routine todo/developer/nightly" || !strings.HasSuffix(e.Title, "with nobody watching") {
		t.Errorf("routine: %+v", e)
	}
}
