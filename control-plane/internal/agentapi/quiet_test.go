package agentapi

import "testing"

func TestAQuietRoutineAnswer(t *testing.T) {
	for _, a := range []string{"NOTHING TO DO", " NOTHING TO DO.\n", "**NOTHING TO DO**", "`NOTHING TO DO`"} {
		if !quiet(a) {
			t.Errorf("%q is not quiet", a)
		}
	}
	for _, a := range []string{"", "Made the short. NOTHING TO DO", "nothing to do here, but I checked the stories", "NOTHING TO DO: but one story failed"} {
		if quiet(a) {
			t.Errorf("%q is quiet", a)
		}
	}
}
