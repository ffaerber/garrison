package agentapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// An agent offers next steps with the gateway's tool suggest, which answers
// "Next step: …" lines. They are kept per chat until its next turn, and are
// what Open WebUI gets when it asks for the chat's follow-ups (the buttons
// under an answer, whose click sends the step as the next message); a chat
// without them gets the LLM's, as before. Channels have no follow-ups.

// FollowUpTask is the X-Garrison-Task of Open WebUI's follow-up request.
const FollowUpTask = "follow_up_generation"

var nextStep = regexp.MustCompile(`(?m)^Next step: (.+)$`)

type nextSteps struct {
	mu    sync.Mutex
	steps map[string]offered
}

type offered struct {
	steps []string
	at    time.Time
}

// set keeps steps for chat; none drops what it had.
func (n *nextSteps) set(chat string, steps []string) {
	if chat == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.steps == nil {
		n.steps = map[string]offered{}
	}
	if len(steps) == 0 {
		delete(n.steps, chat)
		return
	}
	if len(n.steps) > 10000 { // chats long gone
		for c, o := range n.steps {
			if time.Since(o.at) > time.Hour {
				delete(n.steps, c)
			}
		}
	}
	n.steps[chat] = offered{steps, time.Now()}
}

func (n *nextSteps) get(chat string) []string {
	if chat == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.steps[chat].steps
}

// followUps answers Open WebUI's follow-up request of a chat whose agent
// offered next steps with those, not the LLM's guesses; false: it has none,
// or the request is another task.
func (s *Server) followUps(w http.ResponseWriter, r *http.Request, req chatRequest) bool {
	steps := s.next.get(r.Header.Get(ChatHeader))
	if r.Header.Get(TaskHeader) != FollowUpTask || len(steps) == 0 {
		return false
	}
	b, _ := json.Marshal(map[string]any{"follow_ups": steps})
	out := newWriter(w, req.Model, req.Stream)
	out.content(string(b))
	out.finish()
	return true
}
