package changes

import (
	"errors"
	"strings"
	"testing"
)

func TestWhatFilesAreFor(t *testing.T) {
	for _, c := range []struct{ repo, path, want string }{
		{"config", "routines.yaml", "what agents do on their own"},
		{"config", "grants.yaml", "what agents may do here"},
		{"config", "members.yaml", "who belongs here"},
		{"config", "stacks/shop.yaml", "how the app shop is deployed"},
		{"config", "restores/x.yaml", "brings an app's data back"},
		{"config", "agents/media/prompt.md", "the agent media"},
		{"app", ".gitea/workflows/nightly.yaml", "a pipeline or scheduled job"},
		{"app", "docs/shots/home.png", "a picture"},
		{"app", "stacks/x.yaml", ""}, // only config's stacks deploy
		{"app", "main.go", ""},
	} {
		if got := (File{Path: c.path}).What(c.repo); !strings.Contains(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("%s %s: %q, want %q", c.repo, c.path, got, c.want)
		}
	}
}

func TestWhatACardWaitsFor(t *testing.T) {
	pending := []Check{{Name: "garrison/approval", State: "pending"}}
	passed := []Check{{Name: "garrison/approval", State: "success"}}
	for _, c := range []struct {
		card  Card
		waits string
		ready bool
	}{
		{Card{Checks: pending}, "yours, if you may give it", false},
		{Card{Mine: true, Checks: pending}, "from someone who did not propose it", false},
		{Card{Mine: true, Approvals: []string{"bob"}, Checks: []Check{{State: "pending"}}}, "still running", false},
		{Card{Mine: true, Approvals: []string{"bob"}, Checks: passed}, "can be merged", true},
		{Card{Approved: true, Approvals: []string{"bob"}}, "can be merged", true},
		{Card{Mine: true, Approvals: []string{"bob"}, Checks: []Check{{State: "failure"}}}, "A check failed", false},
		{Card{Merged: true}, "It is merged", false},
		{Card{Closed: true}, "closed without merging", false},
	} {
		if got := c.card.Waits(); !strings.Contains(got, c.waits) {
			t.Errorf("%+v waits: %q, want %q", c.card, got, c.waits)
		}
		if c.card.Ready() != c.ready {
			t.Errorf("%+v ready: %v", c.card, !c.ready)
		}
	}
}

func TestDeploys(t *testing.T) {
	if !(Card{Repo: "config", Files: []File{{Path: "stacks/a.yaml"}}}).Deploys() || (Card{Repo: "config", Files: []File{{Path: "routines.yaml"}}}).Deploys() {
		t.Error("config: only stacks and restores deploy")
	}
	if !(Card{Repo: "app", Base: "dev"}).Deploys() || (Card{Repo: "app", Base: "feature"}).Deploys() {
		t.Error("apps: main and dev deploy")
	}
}

func TestPlain(t *testing.T) {
	for _, c := range []struct{ err, want string }{
		{"gitea POST x: 405 Does not have enough approvals", "needs an approval first"},
		{"gitea POST x: 405 Not all required status checks successful", "checks have not all passed"},
		{"gitea POST x: 422 approve your own pull is not allowed", "someone else approves it"},
		{"not allowed", "You may not do that here"},
		{"gitea POST x: 500 boom", "Gitea said: gitea POST x: 500 boom"},
	} {
		if got := Plain(errors.New(c.err)); !strings.Contains(got, c.want) {
			t.Errorf("%q: %q", c.err, got)
		}
	}
}
