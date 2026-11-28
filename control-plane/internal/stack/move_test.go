package stack

import "testing"

func TestParseMove(t *testing.T) {
	m, err := ParseMove([]byte("stack: todo\nto: node2\n"))
	if err != nil || m.Environment != MainEnvironment || m.To != "node2" {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{"stack: todo\n", "stack: ../x\nto: node2\n", "stack: todo\nto: Node_2\n", "stack: todo\nto: n\nwhere: x\n"} {
		if _, err := ParseMove([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
