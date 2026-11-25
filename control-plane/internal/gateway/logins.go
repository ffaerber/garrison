package gateway

// Site logins: a unit keeps accounts on web sites as secrets login-<name>
// (the dashboard's Secrets page), and says in logins.yaml of its config
// which agent may sign in with which, for whose requests. The deployer
// writes them, decrypted, to a folder only the gateway reads; browse types
// a login's username or password into a field of the login's own site, and
// the agent never sees the password. See docs/agents.md, "Site logins".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"garrison/control-plane/internal/agents"
	"garrison/control-plane/internal/secrets"
)

// LoginsFile in a unit's config names who may use its logins.
const LoginsFile = "logins.yaml"

// LoginUse is one entry of logins.yaml: agent may sign in with login, for
// requests of for (admins, members or everyone), and in routines with
// nobody watching if unattended.
type LoginUse struct {
	Login      string `yaml:"login"`
	Agent      string `yaml:"agent"`
	For        string `yaml:"for"`
	Unattended bool   `yaml:"unattended"`
}

// ParseLogins reads logins.yaml.
func ParseLogins(data string) ([]LoginUse, error) {
	var uses []LoginUse
	if err := yaml.Unmarshal([]byte(data), &uses); err != nil {
		return nil, fmt.Errorf("%s: %w", LoginsFile, err)
	}
	for i, u := range uses {
		if !secrets.LoginName.MatchString(u.Login) || u.Agent == "" {
			return nil, fmt.Errorf("%s: [%d]: needs login (a name of a secret login-<name>) and agent", LoginsFile, i)
		}
		switch u.For {
		case "":
			uses[i].For = agents.ForMembers
		case agents.ForAdmins, agents.ForMembers, agents.ForEveryone:
		default:
			return nil, fmt.Errorf("%s: [%d]: for is admins, members or everyone, not %q", LoginsFile, i, u.For)
		}
	}
	return uses, nil
}

// login finds the login name of agent's unit that p's request may use: a
// line of logins.yaml for the agent and the person's role (in a routine,
// one with unattended: true, for its owner), and the login itself.
func (t *Tools) login(ctx context.Context, p Person, agent *agents.Agent, name string) (secrets.Login, error) {
	if agent == nil {
		return secrets.Login{}, refused{"logins are for agents"}
	}
	if t.LoginsDir == "" {
		return secrets.Login{}, refused{"this installation keeps no site logins"}
	}
	if !secrets.LoginName.MatchString(name) {
		return secrets.Login{}, fmt.Errorf("%w: login %q", ErrInvalid, name)
	}
	ns := agent.Unit
	// No logins.yaml: no agent may use a login.
	var raw string
	status, err := t.Gitea.do(ctx, "", http.MethodGet, fmt.Sprintf("/repos/%s/%s/raw/%s", ns, ConfigRepo, LoginsFile), nil, &raw)
	if err != nil && status != http.StatusNotFound {
		return secrets.Login{}, err
	}
	uses, err := ParseLogins(raw)
	if err != nil {
		return secrets.Login{}, err
	}
	person := p.Name
	if p.Unattended {
		person = p.Owner
	}
	role, err := t.Gitea.Role(ctx, ns, person)
	if err != nil {
		return secrets.Login{}, err
	}
	ok := slices.ContainsFunc(uses, func(u LoginUse) bool {
		if u.Login != name || agents.NameOf(u.Agent) != agent.Name || (p.Unattended && !u.Unattended) {
			return false
		}
		switch u.For {
		case agents.ForEveryone:
			return true
		case agents.ForAdmins:
			return role == "admin"
		}
		return role == "admin" || role == "member"
	})
	if !ok {
		return secrets.Login{}, refused{fmt.Sprintf("%s may not use the login %s of %s for %s: %s in %s/config does not allow it. Say so; an admin of %s can add it", agent.Name, name, ns, person, LoginsFile, ns, ns)}
	}
	data, err := os.ReadFile(filepath.Join(t.LoginsDir, ns, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return secrets.Login{}, refused{fmt.Sprintf("%s has no login %s: an admin sets it on the dashboard's Secrets page", ns, name)}
	}
	if err != nil {
		return secrets.Login{}, err
	}
	var l secrets.Login
	if err := json.Unmarshal(data, &l); err != nil || l.Host() == "" {
		return secrets.Login{}, fmt.Errorf("the login %s of %s cannot be read", name, ns)
	}
	return l, nil
}

// loginAction is browse's step that types part (username or password) of
// login into the field n.
func (t *Tools) loginAction(ctx context.Context, p Person, agent *agents.Agent, a args) (Action, error) {
	l, err := t.login(ctx, p, agent, a.Login)
	if err != nil {
		return Action{}, err
	}
	if a.TypeInto <= 0 {
		return Action{}, fmt.Errorf("%w: with login, name the field in type_into", ErrInvalid)
	}
	act := Action{Type: a.TypeInto, Submit: a.Submit, Site: &l}
	switch strings.ToLower(a.Part) {
	case "username", "":
		act.Text = l.Username
	case "password":
		act.Text, act.Password = l.Password, true
	default:
		return Action{}, fmt.Errorf("%w: part is username or password", ErrInvalid)
	}
	return act, nil
}
