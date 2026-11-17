package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A question names its person the way Open WebUI recognises a mention, goes
// into the thread it is given, and the thread's replies come back in order.
func TestPostsMentionAndThreads(t *testing.T) {
	var posted []map[string]string
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/bob":
			io.WriteString(w, `{"email":"Bob@home.test"}`)
		case "/api/v1/users/carol":
			io.WriteString(w, `{"email":"carol@home.test"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gitea.Close()
	owui := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auths/signin":
			io.WriteString(w, `{"token":"t"}`)
		case "/api/v1/auths/":
			io.WriteString(w, `{}`)
		case "/api/v1/channels/list":
			io.WriteString(w, `[{"id":"ch-1","meta":{"`+ChannelMarker+`":"homepage"}}]`)
		case "/api/v1/users/all":
			io.WriteString(w, `{"users":[{"id":"u-bob","email":"bob@home.test"}]}`)
		case "/api/v1/channels/ch-1/messages/post":
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			posted = append(posted, b)
			io.WriteString(w, `{"id":"m-9"}`)
		case "/api/v1/channels/ch-1/messages/m-1/thread":
			// Newest first, as Open WebUI gives them.
			io.WriteString(w, `[{"id":"r2","user_id":"u-bob","content":"later","created_at":2},{"id":"r1","user_id":"u-x","content":"first","created_at":1}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer owui.Close()
	o := &OpenWebUI{URL: owui.URL, Gitea: &Gitea{URL: gitea.URL, User: "admin", Password: "pw"}}
	ctx := context.Background()

	p, err := o.Post(ctx, Post{Agent: "homepage/dev", Text: "Blue or green?", Mention: "bob"})
	if err != nil || p.ID != "m-9" || p.Channel != "ch-1" || p.User != "u-bob" {
		t.Fatalf("posted %+v %v", p, err)
	}
	if got := posted[0]["content"]; got != "**dev**\n\n<@U:u-bob|bob> Blue or green?" || posted[0]["parent_id"] != "" {
		t.Errorf("content %q", got)
	}
	if _, err := o.Post(ctx, Post{Agent: "homepage/dev", Channel: "ch-1", Parent: "m-1", Text: "Done."}); err != nil || posted[1]["parent_id"] != "m-1" {
		t.Errorf("in the thread: %v %v", posted[1], err)
	}
	// Someone who never opened the chat cannot be named.
	if _, err := o.Post(ctx, Post{Agent: "homepage/dev", Text: "?", Mention: "carol"}); !errors.Is(err, ErrNoChatUser) {
		t.Errorf("carol: %v", err)
	}

	thread, err := o.Thread(ctx, "ch-1", "m-1")
	if err != nil || len(thread) != 2 || thread[0].ID != "r1" || thread[1].User != "u-bob" {
		t.Errorf("thread %+v %v", thread, err)
	}
	if _, err := o.Thread(ctx, "ch-1", "../x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a made-up message id: %v", err)
	}
}
