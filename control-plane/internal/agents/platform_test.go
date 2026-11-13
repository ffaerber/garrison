package agents

import (
	"strings"
	"testing"

	"homecloud/control-plane/internal/stack"
)

// The stack.yaml the agents are shown must be one the deployer takes, or
// they copy its mistakes.
func TestThePlatformsExampleStackIsValid(t *testing.T) {
	_, rest, ok := strings.Cut(Platform, "- stack.yaml, in full (nothing else is allowed):\n")
	if !ok {
		t.Fatal("no example in Platform")
	}
	var lines []string
	for _, l := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "    "))
	}
	s, err := stack.Parse([]byte(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(lines, "\n"))
	}
	if s.Name != "immich" || len(s.Services) != 2 || s.Environments["main"].Hostname != "immich" {
		t.Errorf("parsed as %+v", s)
	}
	if !strings.Contains(System(Agent{Namespace: "photos", Name: "operator", Prompt: OperatorPrompt}, "example.com"), "Never put an app into system") {
		t.Error("the system prompt lacks the platform")
	}
}
