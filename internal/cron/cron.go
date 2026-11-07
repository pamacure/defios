// Package cron reads five-field cron specs, as a Schedule's cron is
// written (ADR 0032), and finds when one next fires.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed five-field cron spec: minute, hour, day of month, month
// and day of week, each a set of the values it allows.
type Spec struct {
	minute, hour, dom, month, dow uint64
	// domAny and dowAny are whether the day fields start "*": when neither
	// does, a day either allows is allowed, as in classic cron; else a day
	// both allow.
	domAny, dowAny bool
}

type field struct {
	name     string
	min, max int
	names    []string // names[i] is value min+i
}

var fields = [5]field{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	// 7 is Sunday too, folded onto 0.
	{name: "day of week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// Parse reads spec: five fields separated by spaces, each "*", a value, a
// range "a-b", either with a step "/n", or a comma-separated list of
// those. Months and days of the week may be named by their first three
// letters, in any case.
func Parse(spec string) (Spec, error) {
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf("cron %q: want five fields (minute hour day-of-month month day-of-week), not %d", spec, len(parts))
	}
	var sets [5]uint64
	for i, p := range parts {
		set, err := fields[i].parse(p)
		if err != nil {
			return Spec{}, fmt.Errorf("cron %q: %s: %w", spec, fields[i].name, err)
		}
		sets[i] = set
	}
	if sets[4]&(1<<7) != 0 {
		sets[4] = sets[4]&^(1<<7) | 1
	}
	return Spec{
		minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		domAny: strings.HasPrefix(parts[2], "*"), dowAny: strings.HasPrefix(parts[4], "*"),
	}, nil
}

func (f field) parse(s string) (uint64, error) {
	var set uint64
	for item := range strings.SplitSeq(s, ",") {
		rng, stepText, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("step %q is not a positive number", stepText)
			}
			step = n
		}
		lo, hi := f.min, f.max
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			hi = lo
			if isRange {
				if hi, err = f.value(b); err != nil {
					return 0, err
				}
			} else if stepped {
				hi = f.max // "a/n" runs from a to the end
			}
			if lo > hi {
				return 0, fmt.Errorf("range %q runs backwards", rng)
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f field) value(s string) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", v, f.min, f.max)
	}
	return v, nil
}

// horizon is how far ahead Next looks: past the eight years between some
// leap days.
const horizon = 9 * 366

// errNever is no firing within the horizon.
var errNever = errors.New("never fires")

// Next is the first time after after that s fires, read as wall-clock time
// in loc, or zero if it never does. A time loc's clocks skip does not
// happen, and one they pass twice fires the first time only.
func (s Spec) Next(after time.Time, loc *time.Location) time.Time {
	t, _ := s.next(after, loc)
	return t
}

// Never reports whether s can never fire, as "0 0 30 2 *" cannot.
func (s Spec) Never() bool {
	_, err := s.next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	return err != nil
}

func (s Spec) next(after time.Time, loc *time.Location) (time.Time, error) {
	local := after.In(loc)
	y, m, d := local.Date()
	for i := range horizon {
		day := time.Date(y, m, d+i, 12, 0, 0, 0, loc) // noon: never in a skipped hour
		if !s.allowsDay(day) {
			continue
		}
		dy, dm, dd := day.Date()
		for h := range 24 {
			if s.hour&(1<<h) == 0 {
				continue
			}
			for mi := range 60 {
				if s.minute&(1<<mi) == 0 {
					continue
				}
				t := time.Date(dy, dm, dd, h, mi, 0, 0, loc)
				if t.Hour() != h || t.Minute() != mi || t.Day() != dd {
					continue // the clocks skip it
				}
				t = firstOf(t)
				if !t.After(after) {
					continue // passed, or the second time of one passed
				}
				return t, nil
			}
		}
	}
	return time.Time{}, errNever
}

// firstOf is the first instant whose wall clock reads as t's does: t, or
// an earlier one if the clocks were turned back across it.
func firstOf(t time.Time) time.Time {
	_, now := t.Zone()
	_, before := t.Add(-12 * time.Hour).Zone()
	if before <= now {
		return t
	}
	e := t.Add(-time.Duration(before-now) * time.Second)
	if e.Hour() == t.Hour() && e.Minute() == t.Minute() && e.Day() == t.Day() {
		return e
	}
	return t
}

func (s Spec) allowsDay(day time.Time) bool {
	if s.month&(1<<int(day.Month())) == 0 {
		return false
	}
	dom := s.dom&(1<<day.Day()) != 0
	dow := s.dow&(1<<int(day.Weekday())) != 0
	if s.domAny || s.dowAny {
		return dom && dow // a field starting "*" narrows the other, as in classic cron
	}
	return dom || dow
}
