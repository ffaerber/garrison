package sandbox_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"garrison/control-plane/internal/sandbox"
)

// TestRealSandbox runs a real sandbox image through a real Docker API (e.g.
// the guard), when GARRISON_SANDBOX_TEST is "ADDR IMAGE NETWORK API": API
// must answer as the agent service does, the LLM included.
func TestRealSandbox(t *testing.T) {
	f := strings.Fields(os.Getenv("GARRISON_SANDBOX_TEST"))
	if len(f) != 4 {
		t.Skip("GARRISON_SANDBOX_TEST is not set")
	}
	m := &sandbox.Manager{Engine: &sandbox.Docker{Addr: f[0]}, Image: f[1], Network: f[2], API: f[3], Memory: 1 << 30, CPUs: 1, Pids: 256}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	want := sandbox.Want{Key: "integration", Chat: "test", Agent: "homepage/operator", Person: "alice", Model: "gpt-oss:120b",
		Prompt: "You are homepage/operator. Be brief.", Definition: "1"}
	turn := func(msg string) (answer string, tools []string) {
		err := m.Turn(ctx, want, sandbox.Run{Token: "kc"}, func(s *sandbox.Session) error {
			return s.Pi().Prompt(ctx, msg, nil, func(ev sandbox.Event) {
				switch {
				case ev.Type == "tool_execution_start":
					tools = append(tools, ev.ToolName)
				case ev.Type == "message_end" && ev.Message.Role == "assistant" && ev.Message.StopReason != "toolUse":
					answer = ev.Message.Text() + ev.Message.ErrorMessage
				}
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s → %v %q", msg, tools, answer)
		return
	}
	turn("Use bash to write the word pelican into notes.txt, then say done.")
	// A new container, the same volume: the workspace and the conversation stay.
	if err := m.Clean(ctx); err != nil {
		t.Fatal(err)
	}
	answer, tools := turn("What did you write into notes.txt? Check with bash.")
	if !strings.Contains(strings.ToLower(answer), "pelican") || len(tools) == 0 {
		t.Errorf("answer %q, tools %v", answer, tools)
	}
	if err := m.Clean(ctx); err != nil {
		t.Fatal(err)
	}
}
