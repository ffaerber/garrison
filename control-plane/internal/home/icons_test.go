package home

import (
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/events"
)

// Agents show as @name with a brain, whatever form a title names them in;
// repos, paths and people stay as they are.
func TestTitlesShowAgentsAsAgents(t *testing.T) {
	setAgents([]string{"coder", "commander", "designer", "operator", "scout", "sentry"}, time.Time{})
	for _, c := range []struct {
		e    events.Event
		want []string
		not  []string
	}{
		{events.Event{Title: "todo/commander asked todo/designer, for alice: a short", Actor: "alice"},
			[]string{`@commander</span> asked`, `@designer</span>, for`, `class="pe"`, `working in todo`}, []string{"todo/designer"}},
		{events.Event{Title: "agent-todo-designer pushed to todo/designer@main and todo/app#3"},
			[]string{`@designer</span> pushed to todo/designer@main and todo/app#3`}, []string{"agent-todo-designer"}},
		{events.Event{Title: "bob read /data/media/todo/designer/x.png <b>"},
			[]string{"/data/media/todo/designer/x.png &lt;b&gt;"}, []string{`class="ag"`}},
		{events.Event{Title: "agent-my-unit-scout looked", Actor: "agent-my-unit-scout"},
			[]string{`title="an agent, working in my-unit">`, `@scout</span> looked`}, []string{`class="pe"`}},
	} {
		got := string(titleHTML(c.e))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%q: no %q in %s", c.e.Title, w, got)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%q: %q in %s", c.e.Title, n, got)
			}
		}
	}
	// Every kind has its icon in the sprite, and others a brain.
	for kind, name := range kindIcons {
		if !strings.Contains(string(sprite), `id="i-`+name+`"`) {
			t.Errorf("%s: no icon %s", kind, name)
		}
	}
	if icon(events.Event{Kind: "something-new"}) != "brain" || !strings.Contains(string(sprite), `id="i-brain"`) {
		t.Error("no brain")
	}
}
