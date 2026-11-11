package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRoster(t *testing.T) {
	r, err := ParseRoster([]byte("# comment\nadmins: [alice]\nmembers:\n  - bob\n"))
	if err != nil || r.Role("alice") != "admin" || r.Role("bob") != "member" || r.Role("carol") != "" {
		t.Fatalf("roster = %+v, err %v", r, err)
	}
	for name, doc := range map[string]string{
		"no admin":       "admins: []\nmembers: [bob]\n",
		"listed twice":   "admins: [alice]\nmembers: [alice]\n",
		"not a username": "admins: [\"alice; rm\"]\n",
		"unknown key":    "admins: [alice]\nowners: [bob]\n",
		"not yaml":       "admins: [alice\n",
	} {
		if _, err := ParseRoster([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestChanged(t *testing.T) {
	before := Roster{Admins: []string{"alice"}, Members: []string{"bob"}}
	after := Roster{Admins: []string{"alice", "bob"}, Members: []string{"carol"}}
	if got := strings.Join(Changed(before, after), " "); got != "bob carol" {
		t.Errorf("changed = %q", got)
	}
	if len(Changed(before, before)) != 0 {
		t.Error("no change reported as change")
	}
}

func TestApproval(t *testing.T) {
	base := Roster{Admins: []string{"alice", "carol"}, Members: []string{"bob"}}
	makeBobAdmin := Roster{Admins: []string{"alice", "carol", "bob"}}
	demoteCarol := Roster{Admins: []string{"alice"}, Members: []string{"bob", "carol"}}
	roots := []string{"admin", "alice"}
	for _, c := range []struct {
		name      string
		head      Roster
		author    string
		approvers []string
		ok        bool
	}{
		{"nobody approved", makeBobAdmin, "bob", nil, false},
		{"bob approves his own promotion", makeBobAdmin, "bob", []string{"bob"}, false},
		{"another admin approves bob's promotion", makeBobAdmin, "bob", []string{"carol"}, true},
		{"the author approves", demoteCarol, "alice", []string{"alice"}, false},
		{"carol approves her own demotion", demoteCarol, "alice", []string{"carol"}, false},
		{"a member approves", demoteCarol, "alice", []string{"bob"}, false},
		{"a root admin approves", demoteCarol, "alice", []string{"admin"}, true},
		{"one good approval among bad ones", demoteCarol, "alice", []string{"carol", "bob", "admin"}, true},
	} {
		ok, why := Approval(base, c.head, c.author, c.approvers, roots)
		if ok != c.ok {
			t.Errorf("%s: %v (%s)", c.name, ok, why)
		}
	}
	_, why := Approval(base, demoteCarol, "alice", []string{"carol"}, roots)
	if !strings.Contains(why, "carol's own role changes") || !strings.Contains(why, "changes: carol") {
		t.Errorf("reason = %q", why)
	}
}

func TestSameRule(t *testing.T) {
	want := map[string]any{
		"rule_name": "main", "enable_push": false, "required_approvals": 1,
		"merge_whitelist_teams": []string{"admins", RootTeam}, "merge_whitelist_usernames": []string{},
	}
	var have map[string]any
	json.Unmarshal([]byte(`{"rule_name":"main","enable_push":false,"required_approvals":1,
		"merge_whitelist_teams":["root-admins","admins"],"merge_whitelist_usernames":[],"created_at":"x"}`), &have)
	if !sameRule(want, have) {
		t.Fatal("same rule seen as different")
	}
	have["required_approvals"] = float64(2)
	if sameRule(want, have) {
		t.Fatal("changed approvals not seen")
	}
	have["required_approvals"] = float64(1)
	have["merge_whitelist_teams"] = []any{"admins"}
	if sameRule(want, have) {
		t.Fatal("missing team not seen")
	}
	delete(have, "merge_whitelist_teams")
	if sameRule(want, have) {
		t.Fatal("missing setting not seen")
	}
}
