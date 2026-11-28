package agentapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The steps an agent offered are the chat's follow-ups, until its next turn.
func TestNextStepsAreTheChatsFollowUps(t *testing.T) {
	var n nextSteps
	text := "Next step: Have @coder fix the background colour of todo/app\nNext step: Show me a screenshot of the todo app\nThese show as buttons."
	var steps []string
	for _, m := range nextStep.FindAllStringSubmatch(text, -1) {
		steps = append(steps, m[1])
	}
	n.set("c1", steps)
	if got := n.get("c1"); len(got) != 2 || got[0] != "Have @coder fix the background colour of todo/app" {
		t.Fatalf("%q", got)
	}
	if n.get("c2") != nil || n.get("") != nil {
		t.Error("another chat's")
	}
	n.set("c1", nil)
	if n.get("c1") != nil {
		t.Error("kept after a turn without")
	}
}

// Open WebUI's follow-up request for such a chat gets them, as its JSON.
func TestFollowUpRequestGetsTheOfferedSteps(t *testing.T) {
	s := &Server{}
	s.next.set("c1", []string{"Have @coder fix the background colour of todo/app"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"garrison/commander","stream":false,"messages":[{"role":"user","content":"x"}]}`))
	r.Header.Set(TaskHeader, FollowUpTask)
	r.Header.Set(ChatHeader, "c1")
	if !s.followUps(w, r, chatRequest{Model: "garrison/commander"}) {
		t.Fatal("not answered")
	}
	r.Header.Set(TaskHeader, "title_generation")
	if s.followUps(httptest.NewRecorder(), r, chatRequest{}) {
		t.Error("another task answered")
	}
	var resp struct {
		Choices []struct{ Message struct{ Content string } }
	}
	json.NewDecoder(w.Body).Decode(&resp)
	var got struct {
		FollowUps []string `json:"follow_ups"`
	}
	if len(resp.Choices) != 1 || json.Unmarshal([]byte(resp.Choices[0].Message.Content), &got) != nil || len(got.FollowUps) != 1 {
		t.Fatalf("%s", w.Body.String())
	}
}
