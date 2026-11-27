package gateway

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ProposeMember proposes person in a unit's members.yaml with role (admin
// or member), as the person as who invited them: a pull request on the
// unit's config, which the unit's approval rules hold like any other. A
// person listed already with that role is ErrInvalid.
func (g *Gitea) ProposeMember(ctx context.Context, as, unit, person, role, note string) (Change, error) {
	if unit == PlatformUnit {
		return Change{}, fmt.Errorf("%w: %s has no members.yaml; root admins are made in Keycloak", ErrInvalid, PlatformUnit)
	}
	if role != "admin" && role != "member" {
		return Change{}, fmt.Errorf("%w: a role is admin or member", ErrInvalid)
	}
	if !userName.MatchString(person) {
		return Change{}, fmt.Errorf("%w: %q is not a username", ErrInvalid, person)
	}
	raw, err := g.Read(ctx, as, unit, ConfigRepo, MembersFile)
	if err != nil {
		return Change{}, err
	}
	r, err := ParseRoster([]byte(raw))
	if err != nil {
		return Change{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if r.Role(person) == role {
		return Change{}, fmt.Errorf("%w: %s is %s of %s already", ErrInvalid, person, role, unit)
	}
	r.Admins = slices.DeleteFunc(r.Admins, func(n string) bool { return n == person })
	r.Members = slices.DeleteFunc(r.Members, func(n string) bool { return n == person })
	if role == "admin" {
		r.Admins = append(r.Admins, person)
	} else {
		r.Members = append(r.Members, person)
	}
	if r.Members == nil {
		r.Members = []string{}
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		return Change{}, err
	}
	// The file's comments at its top stay.
	var head strings.Builder
	for _, line := range strings.SplitAfter(raw, "\n") {
		if !strings.HasPrefix(line, "#") {
			break
		}
		head.WriteString(line)
	}
	title := fmt.Sprintf("Add %s to %s as %s", person, unit, role)
	desc := fmt.Sprintf("%s accepted %s's invitation to %s, as %s. Merging it gives them the role; it needs the approval of an admin who did not write it.", person, as, unit, role)
	return g.ProposeAs(ctx, as, unit, ConfigRepo, map[string]string{MembersFile: head.String() + string(out)}, title, desc, "", note)
}
