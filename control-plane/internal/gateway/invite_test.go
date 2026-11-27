package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProposeMember(t *testing.T) {
	members := "# Who may change this unit.\n# admins approve.\nadmins: [dave]\nmembers: [erin]\n"
	var proposed, by, title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/repos/todo/config/raw/members.yaml":
			io.WriteString(w, members)
		case r.URL.Path == "/api/v1/repos/todo/config/contents/members.yaml":
			io.WriteString(w, `{"sha":"m1"}`)
		case r.URL.Path == "/api/v1/repos/todo/config/contents" && r.Method == http.MethodPost:
			by = r.Header.Get("Sudo")
			var body struct{ Files []struct{ Content string } }
			json.NewDecoder(r.Body).Decode(&body)
			data, _ := base64.StdEncoding.DecodeString(body.Files[0].Content)
			proposed = string(data)
			io.WriteString(w, "{}")
		case r.URL.Path == "/api/v1/repos/todo/config/pulls" && r.Method == http.MethodPost:
			var body struct{ Title string }
			json.NewDecoder(r.Body).Decode(&body)
			title = body.Title
			io.WriteString(w, `{"number":14}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	g := &Gitea{URL: srv.URL, User: "admin", Password: "pw"}
	ctx := context.Background()

	c, err := g.ProposeMember(ctx, "dave", "todo", "carla", "member", "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseRoster([]byte(proposed))
	if c.Number != 14 || by != "dave" || err != nil || r.Role("carla") != "member" || r.Role("erin") != "member" || r.Role("dave") != "admin" ||
		!strings.HasPrefix(proposed, "# Who may change this unit.\n# admins approve.\n") || title != "Add carla to todo as member" {
		t.Errorf("by %s, #%d %q:\n%s", by, c.Number, title, proposed)
	}
	// erin as admin moves her; one there already, garrison and odd roles are refused.
	if _, err := g.ProposeMember(ctx, "dave", "todo", "erin", "admin", ""); err != nil {
		t.Error(err)
	} else if r, _ := ParseRoster([]byte(proposed)); r.Role("erin") != "admin" {
		t.Errorf("erin:\n%s", proposed)
	}
	for _, c := range [][3]string{{"todo", "erin", "member"}, {"garrison", "carla", "member"}, {"todo", "carla", "owner"}} {
		if _, err := g.ProposeMember(ctx, "dave", c[0], c[1], c[2], ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", c, err)
		}
	}
}
