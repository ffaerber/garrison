package stack

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// RestoreDir holds a unit's restore requests in its config repo:
// restores/<any name>.yaml. Each is carried out once, when it is merged.
const RestoreDir = "restores"

// Restore asks the deployer to bring an environment's data back to a
// snapshot. The environment's services stop while its datasets are copied
// back; the state before is kept as a snapshot, so a restore can be undone.
type Restore struct {
	Stack       string `yaml:"stack"`       // config/stacks/<stack>.yaml
	Environment string `yaml:"environment"` // main if empty
	Snapshot    string `yaml:"snapshot"`    // as list_snapshots names it
}

// SnapshotName is a ZFS snapshot name the deployer or Sanoid gives:
// deploy-…, restore-…, autosnap_….
var SnapshotName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// ParseRestore reads a restore request and checks its fields; whether the
// stack, environment and snapshot exist is for the caller.
func ParseRestore(data []byte) (*Restore, error) {
	var r Restore
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("not a restore: %w", err)
	}
	if r.Environment == "" {
		r.Environment = MainEnvironment
	}
	var why []error
	if !fileName.MatchString(r.Stack) {
		why = append(why, fmt.Errorf("stack must name a stack of this unit, as in stacks/<stack>.yaml"))
	}
	if !fileName.MatchString(r.Environment) {
		why = append(why, fmt.Errorf("environment %q is not an environment name", r.Environment))
	}
	if !SnapshotName.MatchString(r.Snapshot) {
		why = append(why, fmt.Errorf("snapshot must be a snapshot's name, as list_snapshots shows it"))
	}
	return &r, errors.Join(why...)
}
