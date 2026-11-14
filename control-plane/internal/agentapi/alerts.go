package agentapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"homecloud/control-plane/internal/events"
)

// Alert is one alert of Alertmanager's webhook.
type Alert struct {
	Status       string            `json:"status"` // firing, resolved
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}

// SystemNamespace gets the alerts that name no namespace.
const SystemNamespace = "system"

func (a Alert) namespace() string {
	if ns := a.Labels["namespace"]; ns != "" {
		return ns
	}
	return SystemNamespace
}

// AlertHandler takes Alertmanager's webhook, which must send token as its
// bearer token: the sandboxes can reach this service too.
func (r *Routines) AlertHandler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg struct {
			Alerts []Alert `json:"alerts"`
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
		if err == nil {
			err = json.Unmarshal(body, &msg)
		}
		if err != nil {
			http.Error(w, "not an Alertmanager webhook", http.StatusBadRequest)
			return
		}
		for _, a := range msg.Alerts {
			r.Alert(req.Context(), a)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// matches tells whether every label value the routine asks for is the alert's.
func matches(want map[string]string, a Alert) bool {
	for k, v := range want {
		if a.Labels[k] != v {
			return false
		}
	}
	return true
}

// Alert records an alert in the feed and, when it fires, starts the
// routines of its namespace's agents that wait for it, once per firing.
func (r *Routines) Alert(ctx context.Context, a Alert) {
	ns := a.namespace()
	name := a.Labels["alertname"]
	summary := a.Annotations["summary"]
	if summary == "" {
		summary = name
	}
	firing := a.Status == "firing"
	firingKey := fmt.Sprintf("alert:%s:%d", a.Fingerprint, a.StartsAt.Unix())

	if firing {
		list, err := r.Server.Agents.All(ctx)
		if err != nil {
			log.Printf("alerts: %s: %v", name, err)
		}
		r.mu.Lock()
		if r.prepare(ctx) {
			for _, ag := range list {
				if ag.Namespace != ns {
					continue
				}
				for _, rt := range ag.Routines {
					if rt.OnAlert == nil || !matches(rt.OnAlert, a) {
						continue
					}
					key := ag.ID() + "/" + rt.Name
					once := key + "#" + firingKey
					if _, done := r.last[once]; done {
						continue // this firing started it before
					}
					why := fmt.Sprintf("the alert %s fired at %s: %s%s", name, a.StartsAt.In(r.loc()).Format("Mon 2 Jan 15:04"), summary, alertFacts(a))
					if !r.start(ctx, ag, rt, key, why) {
						log.Printf("alerts: %s: %s is still running; Alertmanager will send it again", name, key)
						continue
					}
					r.last[once] = r.now()
					r.save(ctx, once)
				}
			}
		}
		r.mu.Unlock()
	}

	if r.Server.Events == nil {
		return
	}
	e := events.Event{
		Source: events.SourceAlerts, Kind: events.KindAlert, Namespace: ns, Audience: events.AudienceMembers,
		Title: name + ": " + summary, Detail: alertDetail(a), At: a.StartsAt, Key: firingKey,
	}
	// The routines it started are in the feed on their own, saying why.
	if firing {
		e.Status = events.StatusFailed
	} else {
		e.Status, e.Title = events.StatusOK, "resolved: "+e.Title
	}
	r.Server.Events.Record(ctx, e)
}

// alertFacts lists the labels that say where, for the routine's prompt.
func alertFacts(a Alert) string {
	var b strings.Builder
	keys := make([]string, 0, len(a.Labels))
	for k := range a.Labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "\n- %s: %s", k, a.Labels[k])
	}
	if d := a.Annotations["description"]; d != "" {
		b.WriteString("\n\n" + d)
	}
	return ". Its labels:" + b.String()
}

func alertDetail(a Alert) string {
	return strings.TrimPrefix(alertFacts(a), ". Its labels:\n")
}
