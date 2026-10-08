// Package schedule parses the five field cron expression a PostgresBackup
// carries in spec.schedule and answers the two questions the reconciler asks
// of it: what the next slot after an instant is, and which slot was the most
// recent one at or before an instant.
//
// Everything here is UTC. A backup schedule that follows a wall clock through
// a daylight saving transition either runs twice or not at all on two days a
// year, and neither is worth the field it would take to configure.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrNoSlot is returned when an expression parses but matches no instant
// inside the search window, for example "0 0 30 2 *". February never has a
// thirtieth, so the expression is syntactically fine and permanently dead.
var ErrNoSlot = errors.New("schedule matches no instant within five years")

// searchYears bounds both directions of the scan. Five covers the longest
// real gap between matches, 29 February, which recurs at most every eight
// years in the Gregorian calendar only across century boundaries that are not
// leap years; 2100 is the next one and it is outside anything this operator
// will be running for.
const searchYears = 5

// field is one cron column as a bitmask. restricted records whether the column
// was written as "*", which the reconciler needs because day of month and day
// of week combine differently depending on it.
type field struct {
	bits       uint64
	restricted bool
}

func (f field) has(v int) bool { return f.bits&(1<<uint(v)) != 0 }

// Schedule is a parsed cron expression. The zero value is not usable; call
// Parse.
type Schedule struct {
	expr   string
	minute field
	hour   field
	dom    field
	month  field
	dow    field
}

// String returns the expression the Schedule was parsed from.
func (s Schedule) String() string { return s.expr }

type bound struct {
	name     string
	min, max int
}

var bounds = []bound{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// Parse reads a five field expression: minute, hour, day of month, month, day
// of week. Each field accepts "*", a number, "a-b", "a-b/n", "*/n", and any
// comma separated list of those.
//
// Month and weekday names are rejected rather than translated, and so are the
// "@daily" style macros. Both are conveniences that cost a lookup table and a
// locale argument, and an operator that accepts "JAN" has to decide what
// "jan" and "January" mean too.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("schedule %q has %d fields, want 5 (minute hour day-of-month month day-of-week)", expr, len(fields))
	}
	parsed := make([]field, 5)
	for i, raw := range fields {
		f, err := parseField(raw, bounds[i])
		if err != nil {
			return Schedule{}, err
		}
		parsed[i] = f
	}
	s := Schedule{
		expr:   strings.Join(fields, " "),
		minute: parsed[0],
		hour:   parsed[1],
		dom:    parsed[2],
		month:  parsed[3],
		dow:    normaliseDOW(parsed[4]),
	}
	// A dead expression is a parse error here rather than a surprise at the
	// first reconcile, because the only signal a reconciler can give for a
	// schedule that never fires is silence.
	if _, err := s.Next(time.Unix(0, 0).UTC()); err != nil {
		return Schedule{}, fmt.Errorf("schedule %q: %w", s.expr, err)
	}
	return s, nil
}

// normaliseDOW folds 7 onto 0. Both spellings of Sunday are in use and cron
// has accepted either since Vixie.
func normaliseDOW(f field) field {
	if f.has(7) {
		f.bits |= 1
		f.bits &^= 1 << 7
	}
	return f
}

func parseField(raw string, b bound) (field, error) {
	if raw == "" {
		return field{}, fmt.Errorf("%s field is empty", b.name)
	}
	var f field
	for _, part := range strings.Split(raw, ",") {
		if err := f.addRange(part, b); err != nil {
			return field{}, err
		}
	}
	if f.bits == 0 {
		return field{}, fmt.Errorf("%s field %q matches nothing", b.name, raw)
	}
	return f, nil
}

func (f *field) addRange(part string, b bound) error {
	spec, stepStr, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s field: step %q in %q must be a positive integer", b.name, stepStr, part)
		}
		step = n
	}

	lo, hi := b.min, b.max
	switch spec {
	case "*":
		// A bare "*" leaves the field unrestricted; "*/n" does not, because
		// it selects a subset.
		if hasStep {
			f.restricted = true
		}
	default:
		f.restricted = true
		loStr, hiStr, isRange := strings.Cut(spec, "-")
		n, err := strconv.Atoi(loStr)
		if err != nil {
			return fmt.Errorf("%s field: %q is not a number", b.name, loStr)
		}
		lo, hi = n, n
		if isRange {
			m, err := strconv.Atoi(hiStr)
			if err != nil {
				return fmt.Errorf("%s field: %q is not a number", b.name, hiStr)
			}
			hi = m
		}
		if hasStep && !isRange {
			// "5/15" is a range from 5 to the top of the field, which is how
			// cron reads it. Writing it as "5-59/15" is clearer and both work.
			hi = b.max
		}
	}
	if lo < b.min || hi > b.max {
		return fmt.Errorf("%s field: %q is outside %d-%d", b.name, spec, b.min, b.max)
	}
	if lo > hi {
		return fmt.Errorf("%s field: range %q runs backwards", b.name, spec)
	}
	for v := lo; v <= hi; v += step {
		f.bits |= 1 << uint(v)
	}
	return nil
}

// matchDay applies the rule that catches everyone: when both day of month and
// day of week are restricted, cron matches a day that satisfies EITHER of
// them, not both. "0 0 1 * 1" is the first of the month and every Monday, so
// it fires five or six times a month, not once or twice a year.
func (s Schedule) matchDay(t time.Time) bool {
	if !s.month.has(int(t.Month())) {
		return false
	}
	dom := s.dom.has(t.Day())
	dow := s.dow.has(int(t.Weekday()))
	switch {
	case s.dom.restricted && s.dow.restricted:
		return dom || dow
	case s.dom.restricted:
		return dom
	case s.dow.restricted:
		return dow
	default:
		return true
	}
}

// Next returns the first matching instant strictly after t, to minute
// precision.
//
// The scan skips a whole day when the day fields do not match and a whole
// hour when the hour does not, so the iteration count is bounded by days plus
// hours plus minutes rather than by minutes alone. A minute by minute walk is
// the obvious way to write this and costs 2.6 million steps on an expression
// that only matches once a year.
func (s Schedule) Next(t time.Time) (time.Time, error) {
	cur := floorMinute(t).Add(time.Minute)
	limit := cur.AddDate(searchYears, 0, 0)
	for cur.Before(limit) {
		switch {
		case !s.matchDay(cur):
			cur = startOfNextDay(cur)
		case !s.hour.has(cur.Hour()):
			cur = startOfNextHour(cur)
		case !s.minute.has(cur.Minute()):
			cur = cur.Add(time.Minute)
		default:
			return cur, nil
		}
	}
	return time.Time{}, ErrNoSlot
}

// Latest returns the most recent matching instant at or before t. The
// reconciler uses it to name the slot it should be working on, which is what
// makes the decision a function of the clock rather than of what the previous
// reconcile happened to do.
func (s Schedule) Latest(t time.Time) (time.Time, error) {
	cur := floorMinute(t)
	limit := cur.AddDate(-searchYears, 0, 0)
	for !cur.Before(limit) {
		switch {
		case !s.matchDay(cur):
			cur = endOfPreviousDay(cur)
		case !s.hour.has(cur.Hour()):
			cur = endOfPreviousHour(cur)
		case !s.minute.has(cur.Minute()):
			cur = cur.Add(-time.Minute)
		default:
			return cur, nil
		}
	}
	return time.Time{}, ErrNoSlot
}

// CountBetween returns how many slots fall in the half open interval
// (after, until], stopping at capAt. The second return value reports that the
// count hit the cap and the real number is higher.
//
// The cap exists because an operator restarted after a fortnight's outage on a
// every-five-minutes schedule has 4,032 slots behind it, and nothing useful is
// done with the exact figure: it goes in a log line and a status field.
func (s Schedule) CountBetween(after, until time.Time, capAt int) (int, bool) {
	n := 0
	cur := after
	for n < capAt {
		next, err := s.Next(cur)
		if err != nil || next.After(until) {
			return n, false
		}
		n++
		cur = next
	}
	return n, true
}

func floorMinute(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

func startOfNextDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
}

func startOfNextHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
}

func endOfPreviousDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Add(-time.Minute)
}

func endOfPreviousHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(-time.Minute)
}
