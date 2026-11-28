package agents

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// What a unit lets other units use of it is in publishes.yaml of its
// config, changed by pull request like the rest, so its admins decide what
// leaves it (docs/ontology.md, Links between units):
//
//	- releases: app          # the releases of the repo app
//	  to: [website]          # may start routines of these units
//
// A routine of website with on_release {unit: todo, repo: app} runs only
// while todo's publishes.yaml names website for app.

// PublishesFile is the file in a unit's config repo.
const PublishesFile = "publishes.yaml"

// Publish is one entry of publishes.yaml.
type Publish struct {
	Releases string   `yaml:"releases"` // a repo of the unit
	To       []string `yaml:"to"`       // units
}

// ParsePublishes reads publishes.yaml; why says what is wrong with it.
func ParsePublishes(data []byte) (out []Publish, why []string) {
	var ps []Publish
	if err := yaml.Unmarshal(data, &ps); err != nil {
		return nil, []string{fmt.Sprintf("%s: %v", PublishesFile, err)}
	}
	for i, p := range ps {
		at := fmt.Sprintf("%s[%d]", PublishesFile, i)
		if !repoName.MatchString(p.Releases) {
			why = append(why, at+": releases names a repo of this unit")
			continue
		}
		if len(p.To) == 0 {
			why = append(why, at+": to names the units it goes to")
			continue
		}
		ok := true
		for _, u := range p.To {
			if !person.MatchString(u) {
				why = append(why, fmt.Sprintf("%s: %q is not a unit", at, u))
				ok = false
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, why
}

// PublishesReleases tells whether ps let unit's routines start on the
// releases of repo.
func PublishesReleases(ps []Publish, repo, unit string) bool {
	return slices.ContainsFunc(ps, func(p Publish) bool { return p.Releases == repo && slices.Contains(p.To, unit) })
}
