package gitops

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"garrison/control-plane/internal/apply"
	"garrison/control-plane/internal/events"
	"garrison/control-plane/internal/stack"
)

// A snapshot on request: a person, or an agent for one, asks for a
// snapshot of an environment's data now, before something risky. It only
// adds a copy and changes no data, so it needs no review, unlike a restore;
// it is named manual-<time>, KeepManuals of them are kept, and the feed
// records who asked.

// ErrNoEnvironment: no environment deployed under that name in that unit.
var ErrNoEnvironment = errors.New("no such environment")

var envName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// TakeSnapshot snapshots the environment deployed as envStack (its hostname,
// e.g. todo or dev-todo) of the unit, for by, and returns the snapshot's
// name; "" if the environment holds no data.
func (c *Config) TakeSnapshot(ctx context.Context, unit, envStack, by string) (string, error) {
	if !envName.MatchString(unit) || !envName.MatchString(envStack) {
		return "", fmt.Errorf("%w: %s/%s", ErrNoEnvironment, unit, envStack)
	}
	// One at a time with the syncs: not in the middle of a restore or a
	// redeploy, and snapsChanged is the sync's too.
	unlock, err := c.lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	state, err := ReadState(c.WorkDir)
	if err != nil {
		return "", err
	}
	var app *App
	for _, a := range state.Apps {
		if a.Unit == unit && a.Stack == envStack {
			a := a
			app = &a
		}
	}
	if app == nil {
		return "", fmt.Errorf("%w: %s has no environment %s", ErrNoEnvironment, unit, envStack)
	}
	file := filepath.Join(c.WorkDir, "apps", unit, "config", "@main", stack.Dir, app.Repo+".yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	s, err := stack.ParseIn(data, unit, app.Repo)
	if err != nil {
		return "", err
	}
	name := apply.ManualSnapshot + apply.SnapshotStamp(time.Now())
	took, err := apply.SnapshotEnv(ctx, c.Run, "", s, app.Environment, name)
	if err != nil {
		return "", err
	}
	if !took {
		return "", nil
	}
	c.snapsChanged = true
	c.record(ctx, events.Event{Kind: events.KindSnapshot, Status: events.StatusOK, Unit: unit, Repo: app.Repo,
		Actor: by, StartedBy: by, Audience: events.AudienceMembers,
		Title: fmt.Sprintf("%s took the snapshot %s of %s", by, name, envStack)})
	return name, nil
}

// SnapshotHandler serves POST /snapshots/{unit}/{stack} for the gateway,
// with the service key: {"by": person}; it answers {"snapshot": name}.
func (c *Config) SnapshotHandler(key string, header string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !hmac.Equal([]byte(r.Header.Get(header)), []byte(key)) {
			http.Error(w, "only for the gateway", http.StatusUnauthorized)
			return
		}
		var in struct {
			By string `json:"by"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.By == "" {
			http.Error(w, "who asks?", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		name, err := c.TakeSnapshot(ctx, r.PathValue("unit"), r.PathValue("stack"), in.By)
		switch {
		case errors.Is(err, ErrNoEnvironment):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"snapshot": name})
	})
}
