package stack

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// MoveDir holds a unit's move requests in its config repo:
// moves/<any name>.yaml. Each is carried out once it is merged, and again
// only if the environment is elsewhere than it says.
const MoveDir = "moves"

// Move asks the deployer to move an environment, with its data, to another
// machine. Its services stop while its datasets are copied there; the old
// copy is kept on the old machine, renamed, as a way back.
type Move struct {
	Stack       string `yaml:"stack"`       // config/stacks/<stack>.yaml
	Environment string `yaml:"environment"` // main if empty
	To          string `yaml:"to"`          // the machine, as Kubernetes names its node
}

// nodeName is a Kubernetes node's name.
var nodeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ParseMove reads a move request and checks its fields; whether the stack,
// environment and machine exist is for the caller.
func ParseMove(data []byte) (*Move, error) {
	var m Move
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a move: %w", err)
	}
	if m.Environment == "" {
		m.Environment = MainEnvironment
	}
	var why []error
	if !fileName.MatchString(m.Stack) {
		why = append(why, fmt.Errorf("stack must name a stack of this unit, as in stacks/<stack>.yaml"))
	}
	if !fileName.MatchString(m.Environment) {
		why = append(why, fmt.Errorf("environment %q is not an environment name", m.Environment))
	}
	if !nodeName.MatchString(m.To) {
		why = append(why, fmt.Errorf("to must name a machine, as the dashboard lists them"))
	}
	return &m, errors.Join(why...)
}
