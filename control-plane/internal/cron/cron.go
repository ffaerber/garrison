// Package cron reads schedules in the five fields of cron, minute hour
// day-of-month month day-of-week, e.g. "30 6 * * mon-fri", or one of
// @hourly, @daily (@midnight), @weekly, @monthly, @yearly (@annually).
// A field takes *, a number or name, a range a-b, a step */n or a-b/n, and
// lists of these. As in cron, when both day fields are restricted, a day
// matching either counts. Times are read in the schedule's location.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed cron schedule.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit i: value i is in the field
	domAny, dowAny                bool
	Spec                          string
}

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	months = []string{"", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	days   = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
)

type field struct {
	name     string
	min, max int
	names    []string
}

var fields = []field{
	{"minute", 0, 59, nil},
	{"hour", 0, 23, nil},
	{"day of month", 1, 31, nil},
	{"month", 1, 12, months},
	{"day of week", 0, 7, days}, // 7 is Sunday too
}

// Parse reads spec.
func Parse(spec string) (*Schedule, error) {
	spec = strings.TrimSpace(spec)
	expanded := spec
	if strings.HasPrefix(spec, "@") {
		m, ok := macros[strings.ToLower(spec)]
		if !ok {
			return nil, fmt.Errorf("%q: unknown; @hourly, @daily, @weekly, @monthly and @yearly are known", spec)
		}
		expanded = m
	}
	parts := strings.Fields(expanded)
	if len(parts) != 5 {
		return nil, fmt.Errorf("%q: want five fields, minute hour day-of-month month day-of-week, e.g. 30 6 * * *", spec)
	}
	s := &Schedule{Spec: spec}
	out := []*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, p := range parts {
		bits, err := parseField(p, fields[i])
		if err != nil {
			return nil, fmt.Errorf("%q: %s: %w", spec, fields[i].name, err)
		}
		*out[i] = bits
	}
	if s.dow&(1<<7) != 0 {
		s.dow = s.dow&^(1<<7) | 1
	}
	s.domAny = parts[2] == "*" || parts[2] == "*/1"
	s.dowAny = parts[4] == "*" || parts[4] == "*/1"
	return s, nil
}

func parseField(p string, f field) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(p, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%q: the step must be a positive number", part)
			}
			step = n
		}
		lo, hi := f.min, f.max
		if f.name == "day of week" {
			hi = 6
		}
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = value(a, f); err != nil {
				return 0, err
			}
			if hi, err = value(b, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%q: the range runs backwards", part)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func value(s string, f field) (int, error) {
	for i, n := range f.names {
		if n != "" && strings.EqualFold(s, n) {
			return i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%q is not a %s (%d-%d)", s, f.name, f.min, f.max)
	}
	return v, nil
}

func has(bits uint64, v int) bool { return bits&(1<<v) != 0 }

func (s *Schedule) day(t time.Time) bool {
	dom, dow := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	switch {
	case s.domAny && s.dowAny:
		return true
	case s.domAny:
		return dow
	case s.dowAny:
		return dom
	}
	return dom || dow
}

// Next is the first time after t that the schedule fires, in loc; the zero
// time if it never does (e.g. 30 February) within five years. The search
// runs on the wall clock: a time a clock change skips (02:30 the night
// summer time starts) fires right after the change; one it repeats, once.
func (s *Schedule) Next(t time.Time, loc *time.Location) time.Time {
	after := t
	w := t.In(loc)
	wall := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, time.UTC)
	for {
		next := s.nextWall(wall)
		if next.IsZero() {
			return next
		}
		real := time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), next.Minute(), 0, 0, loc)
		if real.After(after) {
			return real
		}
		wall = next
	}
}

// nextWall searches the wall clock, kept as UTC, which has no clock changes.
func (s *Schedule) nextWall(t time.Time) time.Time {
	loc := time.UTC
	t = t.Add(time.Minute)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		switch {
		case !has(s.month, int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.day(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !has(s.hour, t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// MinGap is the shortest time between two runs in the coming year.
func (s *Schedule) MinGap(from time.Time, loc *time.Location) time.Duration {
	gap := time.Duration(1<<63 - 1)
	prev := s.Next(from, loc)
	if prev.IsZero() {
		return gap
	}
	end := from.AddDate(1, 0, 0)
	for i := 0; i < 20000 && prev.Before(end); i++ {
		next := s.Next(prev, loc)
		if next.IsZero() {
			break
		}
		gap = min(gap, next.Sub(prev))
		prev = next
	}
	return gap
}
