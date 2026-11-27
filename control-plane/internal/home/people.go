package home

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"garrison/control-plane/internal/events"
)

// The People page makes links for people, which the inviter sends them by
// hand, as Garrison sends no mail: an invite makes a new person, and adds
// them to a unit with a role if it names one, by a pull request on the
// unit's members.yaml in the inviter's name, which the unit's approval
// rules hold; a reset link sets a person's password. Root admins invite
// anyone and make reset links; a unit's admins invite into their unit. The
// link opens /join, without signing in: only its token lets one in, once,
// for KeepInvites. Only a hash of the token is kept.

// People is what the People page needs of Keycloak and Gitea.
type People interface {
	// List is everyone who signs in, without the platform's account.
	List(ctx context.Context) ([]Person, error)
	Add(ctx context.Context, username, email, password string) error
	SetPassword(ctx context.Context, username, password string) error
	// ProposeMember proposes person in unit's members.yaml with role, as
	// as, and returns the pull request's number.
	ProposeMember(ctx context.Context, as, unit, person, role string) (int, error)
}

// Person is someone who signs in.
type Person struct {
	Username, Email string
	Root, Enabled   bool
}

// ErrPeople: a request the People page refuses, said in its words.
var ErrPeople = errors.New("people")

type peopleView struct {
	Root    bool // the viewer is a root admin
	Units   []string
	People  []Person
	Invites []inviteRow
	Link    string // a link just made, shown once
	LinkFor string
	Notice  string
	Error   string
}

type inviteRow struct {
	events.Invite
	Expires string
}

type joinView struct {
	Token    string
	Invite   events.Invite
	Domain   string
	Error    string
	Done     bool
	Username string
	PR       int
}

// newToken is a link's token and the hash kept of it.
func newToken() (string, string) {
	b := make([]byte, 32)
	rand.Read(b)
	t := hex.EncodeToString(b)
	return t, hashToken(t)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Server) peopleView(ctx context.Context, v events.Visibility) (*peopleView, error) {
	pv := &peopleView{Root: v.All}
	if s.Secrets != nil {
		units, err := s.Secrets.Units(ctx, v.Person)
		if err != nil {
			return nil, err
		}
		pv.Units = slices.DeleteFunc(units, func(u string) bool { return u == events.PlatformUnit })
	}
	if v.All {
		list, err := s.People.List(ctx)
		if err != nil {
			return nil, err
		}
		pv.People = list
	}
	open, err := s.Invites.OpenInvites(ctx, v.All, pv.Units, v.Person)
	if err != nil {
		return nil, err
	}
	for _, in := range open {
		pv.Invites = append(pv.Invites, inviteRow{Invite: in, Expires: in.Expires.Local().Format("2006-01-02 15:04")})
	}
	return pv, nil
}

func (s *Server) peoplePage(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	pv, err := s.peopleView(r.Context(), v)
	if err != nil {
		log.Printf("home: people: %v", err)
		http.Error(w, "the people cannot be listed right now; try again", http.StatusServiceUnavailable)
		return
	}
	pv.Notice = r.URL.Query().Get("notice")
	s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), PeoplePage: pv})
}

// mayInvite tells whether v may make a link into unit ("" for none).
func (s *Server) mayInvite(ctx context.Context, v events.Visibility, unit string) (bool, error) {
	if v.All {
		return true, nil
	}
	if unit == "" || unit == events.PlatformUnit || s.Secrets == nil {
		return false, nil
	}
	return s.Secrets.May(ctx, unit, v.Person)
}

// peoplePost makes an invite (action=invite: unit, role, note), a reset
// link (action=reset: username; root admins), or revokes a link
// (action=revoke: id).
func (s *Server) peoplePost(w http.ResponseWriter, r *http.Request, v events.Visibility) {
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from the dashboard's own page", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is too long", http.StatusRequestEntityTooLarge)
		return
	}
	f := r.PostForm
	ctx := r.Context()
	render := func(status int, link, linkFor, msg string) {
		pv, err := s.peopleView(ctx, v)
		if err != nil {
			http.Error(w, "the people cannot be listed right now; try again", http.StatusServiceUnavailable)
			return
		}
		pv.Link, pv.LinkFor, pv.Error = link, linkFor, msg
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		s.render(w, "page", feedPage{Who: v.Person, Domain: s.Domain, All: v.All, Now: s.now(), PeoplePage: pv})
	}
	record := func(title string, unit string) {
		if unit == "" {
			unit = events.PlatformUnit
		}
		s.Events.Record(ctx, events.Event{Source: events.SourceHome, Kind: events.KindPeople, Status: events.StatusOK,
			Unit: unit, Audience: events.AudienceMembers, Actor: v.Person, StartedBy: v.Person, Title: title,
			Link: "https://" + s.Domain + "/people"})
	}
	now := s.now()
	switch f.Get("action") {
	case "invite":
		unit, role, note := strings.TrimSpace(f.Get("unit")), f.Get("role"), strings.TrimSpace(f.Get("note"))
		if unit == "" {
			role = ""
		} else if role != "admin" && role != "member" {
			render(http.StatusBadRequest, "", "", "A role is admin or member.")
			return
		}
		ok, err := s.mayInvite(ctx, v, unit)
		if err != nil {
			render(http.StatusServiceUnavailable, "", "", err.Error())
			return
		}
		if !ok {
			msg := "Only root admins invite people without a unit."
			if unit != "" {
				msg = "Only " + unit + "'s admins and root admins invite people into it."
			}
			render(http.StatusForbidden, "", "", msg)
			return
		}
		token, hash := newToken()
		in := events.Invite{Kind: events.InviteNew, Unit: unit, Role: role, Note: note, By: v.Person, At: now, Expires: now.Add(events.KeepInvites)}
		if _, err := s.Invites.MakeInvite(ctx, in, hash); err != nil {
			log.Printf("home: %s: invite: %v", v.Person, err)
			render(http.StatusServiceUnavailable, "", "", "The link could not be made: "+err.Error())
			return
		}
		what := "a new person"
		if unit != "" {
			what = "a new " + role + " of " + unit
		}
		if note != "" {
			what += " (" + note + ")"
		}
		record(fmt.Sprintf("%s invited %s", v.Person, what), unit)
		render(http.StatusOK, "https://"+s.Domain+"/join?token="+token, what, "")
	case "reset":
		name := strings.TrimSpace(f.Get("username"))
		if !v.All {
			render(http.StatusForbidden, "", "", "Only root admins make reset links.")
			return
		}
		list, err := s.People.List(ctx)
		if err != nil {
			render(http.StatusServiceUnavailable, "", "", err.Error())
			return
		}
		if !slices.ContainsFunc(list, func(p Person) bool { return p.Username == name }) {
			render(http.StatusBadRequest, "", "", "There is nobody called "+name+".")
			return
		}
		token, hash := newToken()
		in := events.Invite{Kind: events.InviteReset, Username: name, By: v.Person, At: now, Expires: now.Add(24 * time.Hour)}
		if _, err := s.Invites.MakeInvite(ctx, in, hash); err != nil {
			render(http.StatusServiceUnavailable, "", "", "The link could not be made: "+err.Error())
			return
		}
		record(fmt.Sprintf("%s made a link for %s to set a new password", v.Person, name), "")
		render(http.StatusOK, "https://"+s.Domain+"/join?token="+token, "a new password for "+name, "")
	case "revoke":
		id, _ := strconv.ParseInt(f.Get("id"), 10, 64)
		pv, err := s.peopleView(ctx, v)
		if err != nil {
			render(http.StatusServiceUnavailable, "", "", err.Error())
			return
		}
		// Only a link the viewer sees: theirs, their units', or any for a root admin.
		if !slices.ContainsFunc(pv.Invites, func(i inviteRow) bool { return i.ID == id }) {
			render(http.StatusNotFound, "", "", "There is no such open link.")
			return
		}
		if err := s.Invites.RevokeInvite(ctx, id); err != nil {
			render(http.StatusServiceUnavailable, "", "", err.Error())
			return
		}
		http.Redirect(w, r, "/people?notice=The+link+no+longer+works.", http.StatusSeeOther)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// joinInvite is the open link of the request's token.
func (s *Server) joinInvite(ctx context.Context, token string) (events.Invite, bool, error) {
	if len(token) != 64 {
		return events.Invite{}, false, nil
	}
	return s.Invites.InviteByHash(ctx, hashToken(token))
}

func (s *Server) renderJoin(w http.ResponseWriter, status int, v joinView) {
	v.Domain = s.Domain
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// same-origin: the link's token never leaves for another site, and the
	// form's own request still says where it comes from (no-referrer makes
	// browsers send Origin: null, which the form's check refuses).
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := page.ExecuteTemplate(w, "join", v); err != nil {
		log.Printf("home: join page: %v", err)
	}
}

func (s *Server) joinPage(w http.ResponseWriter, r *http.Request) {
	if s.People == nil || s.Invites == nil {
		http.NotFound(w, r)
		return
	}
	token := r.URL.Query().Get("token")
	in, ok, err := s.joinInvite(r.Context(), token)
	switch {
	case err != nil:
		log.Printf("home: join: %v", err)
		s.renderJoin(w, http.StatusServiceUnavailable, joinView{Error: "The link cannot be checked right now; try again in a moment."})
	case !ok:
		s.renderJoin(w, http.StatusNotFound, joinView{Error: "This link does not work: it was used, revoked, or is older than it may be. Ask whoever sent it for a new one."})
	default:
		s.renderJoin(w, http.StatusOK, joinView{Token: token, Invite: in})
	}
}

// joinPost takes the link's form: a new person's username, email and
// password, or a reset's new password.
func (s *Server) joinPost(w http.ResponseWriter, r *http.Request) {
	if s.People == nil || s.Invites == nil {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Origin") != "https://"+s.Domain {
		http.Error(w, "this form is only taken from its own page", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form is too long", http.StatusRequestEntityTooLarge)
		return
	}
	f := r.PostForm
	ctx := r.Context()
	token := f.Get("token")
	in, ok, err := s.joinInvite(ctx, token)
	if err != nil || !ok {
		s.renderJoin(w, http.StatusNotFound, joinView{Error: "This link does not work: it was used, revoked, or is older than it may be. Ask whoever sent it for a new one."})
		return
	}
	again := func(msg string) {
		s.renderJoin(w, http.StatusBadRequest, joinView{Token: token, Invite: in, Error: msg, Username: f.Get("username")})
	}
	pw := f.Get("password")
	if len(pw) < 12 {
		again("The password needs at least 12 characters.")
		return
	}
	if pw != f.Get("again") {
		again("The two passwords differ.")
		return
	}
	name := in.Username
	if in.Kind == events.InviteNew {
		name = strings.ToLower(strings.TrimSpace(f.Get("username")))
		email := strings.TrimSpace(f.Get("email"))
		if !personName.MatchString(name) || strings.EqualFold(name, s.PlatformUser) || strings.HasPrefix(name, "agent-") {
			again("A username is 3 to 40 lowercase letters, digits, dots, dashes or underscores, starting with a letter.")
			return
		}
		if email == "" {
			email = name + "@" + s.Domain // Gitea needs one; nothing is sent to it
		} else if !validEmail(email) {
			again("That is not an email address. It may also stay empty.")
			return
		}
		// The link is taken first, so a second click cannot make a second person.
		if used, err := s.Invites.UseInvite(ctx, in.ID, name); err != nil || !used {
			s.renderJoin(w, http.StatusConflict, joinView{Error: "This link was used a moment ago."})
			return
		}
		if err := s.People.Add(ctx, name, email, pw); err != nil {
			s.Invites.ReleaseInvite(ctx, in.ID)
			log.Printf("home: join: %s: %v", name, err)
			again("That did not work: " + err.Error())
			return
		}
		view := joinView{Done: true, Username: name, Invite: in}
		title := fmt.Sprintf("%s joined Garrison, invited by %s", name, in.By)
		unit := events.PlatformUnit
		if in.Unit != "" {
			unit = in.Unit
			n, err := s.People.ProposeMember(ctx, in.By, in.Unit, name, in.Role)
			if err != nil {
				log.Printf("home: join: %s into %s: %v", name, in.Unit, err)
			} else {
				view.PR = n
				title += fmt.Sprintf("; %s proposes them as %s of %s", in.By, in.Role, in.Unit)
			}
		}
		s.Events.Record(ctx, events.Event{Source: events.SourceHome, Kind: events.KindPeople, Status: events.StatusOK,
			Unit: unit, Audience: events.AudienceMembers, Actor: name, StartedBy: in.By, Title: title})
		s.renderJoin(w, http.StatusOK, view)
		return
	}
	if used, err := s.Invites.UseInvite(ctx, in.ID, name); err != nil || !used {
		s.renderJoin(w, http.StatusConflict, joinView{Error: "This link was used a moment ago."})
		return
	}
	if err := s.People.SetPassword(ctx, name, pw); err != nil {
		s.Invites.ReleaseInvite(ctx, in.ID)
		log.Printf("home: reset: %s: %v", name, err)
		again("That did not work: " + err.Error())
		return
	}
	s.Events.Record(ctx, events.Event{Source: events.SourceHome, Kind: events.KindPeople, Status: events.StatusOK,
		Unit: events.PlatformUnit, Audience: events.AudienceMembers, Actor: name, StartedBy: in.By,
		Title: fmt.Sprintf("%s set a new password, with a link from %s", name, in.By)})
	s.renderJoin(w, http.StatusOK, joinView{Done: true, Username: name, Invite: in})
}
