package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unit.md of a unit: its people, its apps with their addresses and whether
// they run, and the agents' rights there, as the person may see it.
func TestUnitMD(t *testing.T) {
	m, _ := setup(t)
	md, err := m.Tools.UnitMD(context.Background(), "bob", "homepage")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + md)
	for _, want := range []string{"# homepage", "People: alice (admins); bob (members)", "## Apps", "- todo: https://dev-todo.example.com/ https://todo.example.com/, at fedcba9, running"} {
		if !strings.Contains(md, want) {
			t.Errorf("no %q in\n%s", want, md)
		}
	}
	if _, err := m.Tools.UnitMD(context.Background(), "bob", "nowhere"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a unit bob cannot see: %v", err)
	}
}

func TestHealthInWords(t *testing.T) {
	for _, c := range []struct {
		s    []Service
		want string
	}{
		{nil, "not running"},
		{[]Service{{Name: "web", Replicas: "1/1"}, {Name: "db", Replicas: "1/1"}}, "running"},
		{[]Service{{Name: "web", Replicas: "0/1"}}, "not all running: web 0/1"},
	} {
		if got := health(c.s); got != c.want {
			t.Errorf("%v: %q, want %q", c.s, got, c.want)
		}
	}
}

// take_snapshot: for a person of the unit, asked of the deployer with the
// service key; refused to anyone else.
func TestTakeSnapshotAsksTheDeployer(t *testing.T) {
	m, _ := setup(t)
	var asked, by, key string
	dep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked, key = r.Method+" "+r.URL.Path, r.Header.Get(ServiceHeader)
		var in struct{ By string }
		json.NewDecoder(r.Body).Decode(&in)
		by = in.By
		io.WriteString(w, `{"snapshot":"manual-20261127-180000"}`)
	}))
	defer dep.Close()
	m.Tools.Deployer, m.Tools.ServiceKey = dep.URL, "sk"
	out, err := m.Tools.takeSnapshot(context.Background(), Person{Name: "bob"}, "homepage", "todo")
	if err != nil || !strings.Contains(out, "manual-20261127-180000") || asked != "POST /snapshots/homepage/todo" || by != "bob" || key != "sk" {
		t.Errorf("%q %v; %s by %s with %q", out, err, asked, by, key)
	}
	if _, err := m.Tools.takeSnapshot(context.Background(), Person{Name: "dave"}, "homepage", "todo"); !errors.Is(err, ErrForbidden) {
		t.Errorf("dave, in no unit: %v", err)
	}
}

// A screenshot without a repo is shown in the chat: refused where no
// screenshots are kept, before the browser runs.
func TestAScreenshotToShowNeedsAStore(t *testing.T) {
	m, _ := setup(t)
	m.Tools.Browser = &Browser{URL: "http://127.0.0.1:1", Domain: "example.com"}
	if text, isErr := callText(t, m, "tok-alice", "screenshot", map[string]any{"url": "https://grafana.example.com/"}); !isErr || !strings.Contains(text, "keeps no screenshots to show") {
		t.Errorf("%v %s", isErr, text)
	}
}
