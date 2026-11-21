package system

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every service of the platform runs on the main machine, the swarm's
// manager, unless it runs on every machine (mode: global): their data, the
// Docker socket they use and the files under /etc/garrison are there. A
// machine that joins the swarm runs apps only (docs/nodes.md).
func TestThePlatformStaysOnTheMainMachine(t *testing.T) {
	files, err := filepath.Glob("../../../garrison/*/compose.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no system stacks found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Services map[string]struct {
				Deploy struct {
					Mode      string `yaml:"mode"`
					Placement struct {
						Constraints []string `yaml:"constraints"`
					} `yaml:"placement"`
				} `yaml:"deploy"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for name, s := range c.Services {
			if s.Deploy.Mode != "global" && !slices.Contains(s.Deploy.Placement.Constraints, "node.role == manager") {
				t.Errorf("%s: service %s may run on any machine; give it placement.constraints [node.role == manager]", f, name)
			}
		}
	}
}
