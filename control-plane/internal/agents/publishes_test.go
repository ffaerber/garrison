package agents

import (
	"strings"
	"testing"
)

func TestPublishes(t *testing.T) {
	ps, why := ParsePublishes([]byte("- releases: app\n  to: [website]\n- releases: ../x\n  to: [a]\n- releases: docs\n"))
	if len(ps) != 1 || len(why) != 2 {
		t.Fatalf("%v %v", ps, why)
	}
	if !PublishesReleases(ps, "app", "website") || PublishesReleases(ps, "app", "shop") || PublishesReleases(ps, "docs", "website") {
		t.Error("who gets what")
	}
	if _, why := ParsePublishes([]byte("releases: app")); len(why) != 1 {
		t.Errorf("not a list: %v", why)
	}
}

func TestOnReleaseRoutines(t *testing.T) {
	ok := []Routine{{Name: "news", Owner: "bob", Task: "t", OnRelease: &Release{Unit: "todo", Repo: "app"}}}
	if _, why := checkRoutines(ok); len(why) != 0 {
		t.Errorf("%v", why)
	}
	bad := []Routine{{Name: "news", Owner: "bob", Task: "t", OnRelease: &Release{Unit: "todo"}}}
	if _, why := checkRoutines(bad); len(why) != 1 || !strings.Contains(why[0], "on_release") {
		t.Errorf("%v", why)
	}
	two := []Routine{{Name: "news", Owner: "bob", Task: "t", Raw: "1h", OnRelease: &Release{Repo: "app"}}}
	if _, why := checkRoutines(two); len(why) == 0 {
		t.Error("two triggers accepted")
	}
}
