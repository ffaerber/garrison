package agentapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/gateway"
)

// Each turn tells the agent where it works, as it is now: the unit.md of
// the unit it works in (docs/ontology.md), or, in a private chat, which
// belongs to no unit, of every unit the person is in. It goes into the
// turn's message, not the system prompt, whose change would restart the
// sandbox; and in full only when it changed since the chat's last turn, so
// a long conversation does not carry it again and again.

// UnitDescriber gives unit.md: the gateway's.
type UnitDescriber interface {
	// UnitMD is the unit as person may see it.
	UnitMD(ctx context.Context, unit, person string) (string, error)
	// PersonUnitsMD is unit.md of each unit person is in.
	PersonUnitsMD(ctx context.Context, person string) (string, error)
}

// sentUnits remembers, per chat, a hash of the unit.md its last turn got.
type sentUnits struct {
	mu   sync.Mutex
	last map[string][32]byte
}

func (u *sentUnits) changed(chat string, md string) bool {
	h := sha256.Sum256([]byte(md))
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil || len(u.last) > 10000 {
		u.last = map[string][32]byte{}
	}
	if u.last[chat] == h {
		return false
	}
	u.last[chat] = h
	return true
}

// unitContext is what goes before the turn's message: where the agent
// works, as unit.md. fresh: the session is new, so it gets it in full.
func (s *Server) unitContext(ctx context.Context, chat string, a agents.Agent, person string, fresh bool) string {
	if s.Units == nil || person == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	private := a.Unit == agents.PlatformUnit && !strings.HasPrefix(chat, "channel:")
	var md, what string
	var err error
	if private {
		md, err = s.Units.PersonUnitsMD(ctx, person)
		what = fmt.Sprintf("This chat belongs to no unit. The units %s is in, as they are now (unit.md); \"my unit\" means one of these", person)
	} else {
		md, err = s.Units.UnitMD(ctx, a.Unit, person)
		what = fmt.Sprintf("The unit you work in, %s, as it is now (unit.md)", a.Unit)
	}
	if err != nil {
		log.Printf("agents: unit.md for %s, %s: %v", a.ID(), person, err)
		return ""
	}
	key := chat
	if key == "" {
		key = a.ID() + " " + person
	}
	if !s.sent.changed(key, md) && !fresh {
		return fmt.Sprintf("(%s: unchanged since your last turn.)\n\n", strings.SplitN(what, ",", 2)[0])
	}
	return what + ":\n\n" + strings.TrimSpace(md) + "\n\n---\n\n"
}

func (g *GatewayPoster) markdown(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.URL+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(gateway.ServiceHeader, g.ServiceKey)
	resp, err := g.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

func (g *GatewayPoster) UnitMD(ctx context.Context, unit, person string) (string, error) {
	return g.markdown(ctx, "/internal/units/"+url.PathEscape(unit)+"/unit.md?for="+url.QueryEscape(person))
}

func (g *GatewayPoster) PersonUnitsMD(ctx context.Context, person string) (string, error) {
	return g.markdown(ctx, "/internal/people/"+url.PathEscape(person)+"/units.md")
}
