package home

import (
	"context"
	"strings"
	"testing"

	"garrison/control-plane/internal/events"
)

type fakeUnitPages map[string]string // unit -> unit.md, as bob sees it

func (f fakeUnitPages) UnitMD(_ context.Context, unit, person string) (string, error) {
	md, ok := f[unit]
	if !ok {
		return "", ErrUnitHidden
	}
	return md, nil
}
func (f fakeUnitPages) PersonUnitsMD(_ context.Context, person string) (string, error) {
	return f["todo"], nil
}

func TestTheUnitsPage(t *testing.T) {
	s, _ := server()
	s.Units = fakeUnitPages{"todo": "# todo: Our <b>todo</b> list\n\nPeople: alice (admins); bob (members).\n\n## Apps\n\n- todo: https://todo.home.test/, at fedcba9, running\n\n## Agents' rights and routines here\n\n- @coder: merge on app into dev, for members\n"}
	body := get(s, "bob", "/u").Body.String()
	for _, want := range []string{`href="/u" class="on">Units</a>`, "<h1>todo: Our &lt;b&gt;todo&lt;/b&gt; list</h1>", `<a href="https://todo.home.test/">https://todo.home.test/</a>`, `@coder</span>: merge on app`, "<h2>Apps</h2>"} {
		if !strings.Contains(body, want) {
			t.Errorf("no %q in the page", want)
		}
	}
	if strings.Contains(body, "<b>todo</b>") {
		t.Error("unit.md's HTML is not escaped")
	}
	if body := get(s, "bob", "/u/todo").Body.String(); !strings.Contains(body, `href="/u/todo.md"`) || !strings.Contains(body, "← Your units") {
		t.Errorf("one unit's page:\n%s", body)
	}
	w := get(s, "bob", "/u/todo.md")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.HasPrefix(w.Body.String(), "# todo") {
		t.Errorf("as Markdown: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get(s, "bob", "/u/secret"); w.Code != 404 {
		t.Errorf("a hidden unit: %d", w.Code)
	}
}

// A screenshot shown in a chat: to the person who asked, the unit's people
// and root admins; to nobody else, as if it were not there.
func TestShotsAreServedToWhomMaySee(t *testing.T) {
	s, _ := server()
	shots := &events.MemoryShots{}
	s.Shots = shots
	s.Access = access{"alice": {All: true}, "bob": {Members: map[string]bool{"todo": true}}, "carl": {}, "erin": {Members: map[string]bool{"lab": true}}}
	id, _ := shots.SaveShot(context.Background(), events.Shot{Unit: "todo", Person: "carl", URL: "https://todo.home.test/", PNG: []byte("\x89PNG")})
	for who, want := range map[string]int{"carl": 200, "bob": 200, "alice": 200, "erin": 404} {
		w := get(s, who, "/shots/"+id+".png")
		if w.Code != want {
			t.Errorf("%s: %d, want %d", who, w.Code, want)
		}
		if want == 200 && (w.Header().Get("Content-Type") != "image/png" || w.Body.String() != "\x89PNG") {
			t.Errorf("%s: %s %q", who, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	if w := get(s, "carl", "/shots/nothing.png"); w.Code != 404 {
		t.Errorf("no such shot: %d", w.Code)
	}
}
