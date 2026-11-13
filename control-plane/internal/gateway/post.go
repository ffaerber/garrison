package gateway

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
)

// Post is an answer of an agent that no person's message is waiting for:
// a routine's, or one an agent was asked for in the background. It goes into
// a channel: the one named, or the agent's namespace's.
type Post struct {
	Agent   string `json:"agent"`   // <namespace>/<agent>
	Channel string `json:"channel"` // an Open WebUI channel id; empty for the namespace's
	Text    string `json:"text"`
}

var channelID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Post writes p into its channel, as the gateway's account, under the
// agent's name.
func (o *OpenWebUI) Post(ctx context.Context, p Post) error {
	ns, _, ok := strings.Cut(p.Agent, "/")
	if !ok || validOrg(ns) != nil {
		return fmt.Errorf("%w: agent %q", ErrInvalid, p.Agent)
	}
	if p.Channel != "" && !channelID.MatchString(p.Channel) {
		return fmt.Errorf("%w: channel %q", ErrInvalid, p.Channel)
	}
	token, err := o.session(ctx)
	if err != nil {
		return err
	}
	id := p.Channel
	if id == "" {
		var all []owuiChannel
		if err := o.call(ctx, token, http.MethodGet, "/api/v1/channels/list", nil, &all); err != nil {
			return err
		}
		for _, c := range all {
			if c.Meta[ChannelMarker] == ns {
				id = c.ID
			}
		}
		if id == "" {
			return fmt.Errorf("namespace %s has no channel yet", ns)
		}
	}
	text := strings.TrimSpace(p.Text)
	if len(text) > 30000 {
		text = text[:30000] + "\n[cut]"
	}
	return o.call(ctx, token, http.MethodPost, "/api/v1/channels/"+id+"/messages/post",
		map[string]string{"content": "**" + p.Agent + "**\n\n" + text}, nil)
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
		if err := o.Post(r.Context(), p); err != nil {
			log.Printf("post: %s: %v", p.Agent, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("post: %s in channel %q", p.Agent, p.Channel)
		w.WriteHeader(http.StatusNoContent)
	})
}
