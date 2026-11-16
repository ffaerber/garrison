package gateway

import (
	"cmp"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Post is an answer of an agent that no person's message is waiting for:
// a routine's, or one an agent was asked for in the background. It goes into
// a channel: the one named, or the agent's namespace's.
type Post struct {
	Agent   string `json:"agent"`   // <namespace>/<agent>
	Channel string `json:"channel"` // an Open WebUI channel id; empty for the namespace's
	Text    string `json:"text"`
	// Parent, if set, is the message whose thread it goes into.
	Parent string `json:"parent,omitempty"`
	// Mention, if set, is a person the post names first, so that Open WebUI
	// tells them, even in a channel they muted.
	Mention string `json:"mention,omitempty"`
}

// Posted is a post as Open WebUI keeps it: its message, in its channel, and
// the Open WebUI user of the person it mentions.
type Posted struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	User    string `json:"user,omitempty"`
}

var channelID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Post writes p into its channel, as the gateway's account, under the
// agent's name.
func (o *OpenWebUI) Post(ctx context.Context, p Post) (Posted, error) {
	ns, _, ok := strings.Cut(p.Agent, "/")
	if !ok || validOrg(ns) != nil {
		return Posted{}, fmt.Errorf("%w: agent %q", ErrInvalid, p.Agent)
	}
	if p.Channel != "" && !channelID.MatchString(p.Channel) {
		return Posted{}, fmt.Errorf("%w: channel %q", ErrInvalid, p.Channel)
	}
	if p.Parent != "" && !channelID.MatchString(p.Parent) {
		return Posted{}, fmt.Errorf("%w: message %q", ErrInvalid, p.Parent)
	}
	token, err := o.session(ctx)
	if err != nil {
		return Posted{}, err
	}
	id := p.Channel
	if id == "" {
		var all []owuiChannel
		if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
			return Posted{}, err
		}
		for _, c := range all {
			if c.Meta[ChannelMarker] == ns {
				id = c.ID
			}
		}
		if id == "" {
			return Posted{}, fmt.Errorf("namespace %s has no channel yet", ns)
		}
	}
	text := strings.TrimSpace(p.Text)
	if len(text) > 30000 {
		text = text[:30000] + "\n[cut]"
	}
	out := Posted{Channel: id}
	if p.Mention != "" {
		if out.User, err = o.user(ctx, token, p.Mention); err != nil {
			return Posted{}, err
		}
		text = fmt.Sprintf("<@U:%s|%s> %s", out.User, p.Mention, text)
	}
	body := map[string]string{"content": "**" + p.Agent + "**\n\n" + text}
	if p.Parent != "" {
		body["parent_id"] = p.Parent
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+id+"/messages/post", body, &msg); err != nil {
		return Posted{}, err
	}
	out.ID = msg.ID
	return out, nil
}

// ErrNoChatUser: the person has not signed in to the chat yet.
var ErrNoChatUser = errors.New("has not signed in to the chat yet")

// user is the Open WebUI user of a person, found by their email in Gitea.
func (o *OpenWebUI) user(ctx context.Context, token, person string) (string, error) {
	if !userName.MatchString(person) {
		return "", fmt.Errorf("%w: user %q", ErrInvalid, person)
	}
	var u struct {
		Email string `json:"email"`
	}
	if _, err := o.Gitea.do(ctx, "", http.MethodGet, "/users/"+person, nil, &u); err != nil {
		return "", err
	}
	var users struct {
		Users []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"users"`
	}
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/users/all", nil, &users); err != nil {
		return "", err
	}
	for _, x := range users.Users {
		if u.Email != "" && strings.EqualFold(x.Email, u.Email) {
			return x.ID, nil
		}
	}
	return "", fmt.Errorf("%s %w", person, ErrNoChatUser)
}

// ThreadMessage is a reply in a thread.
type ThreadMessage struct {
	ID      string `json:"id"`
	User    string `json:"user"` // its Open WebUI user
	Content string `json:"content"`
	At      int64  `json:"at"` // nanoseconds since 1970
}

// Thread is the thread under a message, oldest first; the newest 100.
func (o *OpenWebUI) Thread(ctx context.Context, channel, message string) ([]ThreadMessage, error) {
	if !channelID.MatchString(channel) || !channelID.MatchString(message) {
		return nil, fmt.Errorf("%w: channel %q, message %q", ErrInvalid, channel, message)
	}
	token, err := o.session(ctx)
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID        string `json:"id"`
		UserID    string `json:"user_id"`
		Content   string `json:"content"`
		CreatedAt int64  `json:"created_at"`
	}
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/"+channel+"/messages/"+message+"/thread?limit=100", nil, &list); err != nil {
		return nil, err
	}
	out := make([]ThreadMessage, 0, len(list))
	for _, m := range list {
		out = append(out, ThreadMessage{ID: m.ID, User: m.UserID, Content: m.Content, At: m.CreatedAt})
	}
	slices.SortFunc(out, func(a, b ThreadMessage) int { return cmp.Compare(a.At, b.At) })
	return out, nil
}

// ThreadHandler serves GET /internal/threads/{channel}/{message} for the
// agent service: the replies to a question it posted.
func ThreadHandler(o *OpenWebUI, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !hmac.Equal([]byte(r.Header.Get(ServiceHeader)), []byte(key)) {
			http.Error(w, "only for the agent service", http.StatusUnauthorized)
			return
		}
		list, err := o.Thread(r.Context(), r.PathValue("channel"), r.PathValue("message"))
		if errors.Is(err, ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})
}

// PostHandler serves POST /internal/post for the agent service, which
// shows the service key.
func PostHandler(o *OpenWebUI, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !hmac.Equal([]byte(r.Header.Get(ServiceHeader)), []byte(key)) {
			http.Error(w, "only for the agent service", http.StatusUnauthorized)
			return
		}
		var p Post
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
			http.Error(w, "not a post", http.StatusBadRequest)
			return
		}
		posted, err := o.Post(r.Context(), p)
		if errors.Is(err, ErrNoChatUser) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err != nil {
			log.Printf("post: %s: %v", p.Agent, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("post: %s in channel %q", p.Agent, posted.Channel)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(posted)
	})
}

// HasChannel tells whether namespace ns has its channel yet.
func (o *OpenWebUI) HasChannel(ctx context.Context, ns string) (bool, error) {
	token, err := o.session(ctx)
	if err != nil {
		return false, err
	}
	var all []owuiChannel
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
		return false, err
	}
	return slices.ContainsFunc(all, func(c owuiChannel) bool { return c.Meta[ChannelMarker] == ns }), nil
}

// ChannelInfo is what a channel is to the platform.
type ChannelInfo struct {
	Namespace string `json:"namespace"` // empty: not a namespace's
	// Members: only the namespace's people read it. Every namespace's
	// channel but system's.
	Members bool `json:"members"`
}

// Channel tells what the channel id is.
func (o *OpenWebUI) Channel(ctx context.Context, id string) (ChannelInfo, error) {
	token, err := o.session(ctx)
	if err != nil {
		return ChannelInfo{}, err
	}
	var all []owuiChannel
	if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
		return ChannelInfo{}, err
	}
	for _, c := range all {
		if c.ID != id {
			continue
		}
		ns, _ := c.Meta[ChannelMarker].(string)
		return ChannelInfo{Namespace: ns, Members: ns != "" && ns != SystemNamespace}, nil
	}
	return ChannelInfo{}, nil
}

// ChannelHandler serves GET /internal/channels/{id} for the agent service.
func ChannelHandler(o *OpenWebUI, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !hmac.Equal([]byte(r.Header.Get(ServiceHeader)), []byte(key)) {
			http.Error(w, "only for the agent service", http.StatusUnauthorized)
			return
		}
		id := r.PathValue("id")
		if !channelID.MatchString(id) {
			http.Error(w, "not a channel", http.StatusBadRequest)
			return
		}
		info, err := o.Channel(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
}
