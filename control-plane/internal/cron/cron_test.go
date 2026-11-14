package cron

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, berlin)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ spec, from, want string }{
		{"30 6 * * *", "2026-11-14 06:29", "2026-11-14 06:30"},
		{"30 6 * * *", "2026-11-14 06:30", "2026-11-15 06:30"},
		{"@daily", "2026-11-14 13:00", "2026-11-15 00:00"},
		{"@hourly", "2026-11-14 13:00", "2026-11-14 14:00"},
		{"0 9 * * mon-fri", "2026-11-14 10:00", "2026-11-16 09:00"}, // Saturday → Monday
		{"*/15 * * * *", "2026-11-14 10:07", "2026-11-14 10:15"},
		{"0 0 1 * *", "2026-11-14 10:00", "2026-12-01 00:00"},
		{"0 12 13 * fri", "2026-11-14 13:00", "2026-11-20 12:00"}, // either day field
		{"0 8 * dec sun", "2026-11-14 10:00", "2026-12-06 08:00"},
		{"0 3 * * 7", "2026-11-14 10:00", "2026-11-15 03:00"},   // 7 is Sunday
		{"30 2 29 3 *", "2026-03-01 00:00", "2026-03-29 03:30"}, // 02:30 does not exist that night in Berlin
	} {
		s, err := Parse(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := s.Next(at(c.from), berlin).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%s after %s: %s, want %s", c.spec, c.from, got, c.want)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, spec := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "5-1 * * * *", "*/0 * * * *", "@often", "0 0 * * funday"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("%q accepted", spec)
		}
	}
}

func TestNeverAndGaps(t *testing.T) {
	s, _ := Parse("0 0 30 2 *")
	if !s.Next(time.Now(), time.UTC).IsZero() {
		t.Error("30 February came")
	}
	s, _ = Parse("*/5 * * * *")
	if g := s.MinGap(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); g != 5*time.Minute {
		t.Errorf("gap %s", g)
	}
	s, _ = Parse("0 6,18 * * *")
	if g := s.MinGap(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); g != 12*time.Hour {
		t.Errorf("gap %s", g)
	}
}
