package schedule

import (
	"errors"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func mustParse(t *testing.T, expr string) Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) returned %v, want a schedule", expr, err)
	}
	return s
}

func TestNext(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
	}{
		{"0 2 * * *", "2026-03-01T01:59:00Z", "2026-03-01T02:00:00Z"},
		{"0 2 * * *", "2026-03-01T02:00:00Z", "2026-03-02T02:00:00Z"},
		{"*/15 * * * *", "2026-03-01T02:00:30Z", "2026-03-01T02:15:00Z"},
		{"*/15 * * * *", "2026-03-01T02:46:00Z", "2026-03-01T03:00:00Z"},
		{"30 3 * * 0", "2026-03-02T00:00:00Z", "2026-03-08T03:30:00Z"},
		{"30 3 * * 7", "2026-03-02T00:00:00Z", "2026-03-08T03:30:00Z"},
		{"0 0 1 * *", "2026-01-15T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"5,20,50 * * * *", "2026-03-01T00:21:00Z", "2026-03-01T00:50:00Z"},
		{"0 9-17 * * 1-5", "2026-03-06T17:30:00Z", "2026-03-09T09:00:00Z"},
		{"0 0-23/6 * * *", "2026-03-01T07:00:00Z", "2026-03-01T12:00:00Z"},
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		got, err := s.Next(at(c.from))
		if err != nil {
			t.Errorf("%q.Next(%s) returned %v", c.expr, c.from, err)
			continue
		}
		if !got.Equal(at(c.want)) {
			t.Errorf("%q.Next(%s) = %s, want %s", c.expr, c.from, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestNextIsStrictlyAfter(t *testing.T) {
	s := mustParse(t, "*/5 * * * *")
	// Seconds inside the minute must not make Next return the minute it was
	// handed, or a reconcile at 02:00:30 would be handed 02:00 as its next
	// slot and create the Job for the slot it has already run.
	for _, from := range []string{"2026-03-01T02:00:00Z", "2026-03-01T02:00:30Z", "2026-03-01T02:00:59Z"} {
		got, err := s.Next(at(from))
		if err != nil {
			t.Fatalf("Next(%s): %v", from, err)
		}
		if !got.Equal(at("2026-03-01T02:05:00Z")) {
			t.Errorf("Next(%s) = %s, want 02:05", from, got.Format(time.RFC3339))
		}
	}
}

func TestLatest(t *testing.T) {
	cases := []struct {
		expr string
		from string
		want string
	}{
		{"0 2 * * *", "2026-03-01T01:59:00Z", "2026-02-28T02:00:00Z"},
		{"0 2 * * *", "2026-03-01T02:00:00Z", "2026-03-01T02:00:00Z"},
		{"0 2 * * *", "2026-03-01T02:00:45Z", "2026-03-01T02:00:00Z"},
		{"*/15 * * * *", "2026-03-01T02:44:00Z", "2026-03-01T02:30:00Z"},
		{"30 3 * * 0", "2026-03-09T00:00:00Z", "2026-03-08T03:30:00Z"},
		{"0 0 1 1 *", "2026-06-01T00:00:00Z", "2026-01-01T00:00:00Z"},
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		got, err := s.Latest(at(c.from))
		if err != nil {
			t.Errorf("%q.Latest(%s) returned %v", c.expr, c.from, err)
			continue
		}
		if !got.Equal(at(c.want)) {
			t.Errorf("%q.Latest(%s) = %s, want %s", c.expr, c.from, got.Format(time.RFC3339), c.want)
		}
	}
}

func TestLatestAndNextBracketTheInstant(t *testing.T) {
	// Latest and Next must not overlap or leave a gap: for any instant,
	// Latest(t) <= t < Next(t), and Next(Latest(t)) is the slot after it.
	s := mustParse(t, "17 */3 * * *")
	cur := at("2026-05-04T00:00:00Z")
	for i := 0; i < 200; i++ {
		latest, err := s.Latest(cur)
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		next, err := s.Next(cur)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if latest.After(cur) {
			t.Fatalf("Latest(%s) = %s is in the future", cur, latest)
		}
		if !next.After(cur) {
			t.Fatalf("Next(%s) = %s is not after it", cur, next)
		}
		afterLatest, err := s.Next(latest)
		if err != nil {
			t.Fatalf("Next(Latest): %v", err)
		}
		if !afterLatest.Equal(next) {
			t.Fatalf("Next(Latest(%s)) = %s but Next(%s) = %s", cur, afterLatest, cur, next)
		}
		cur = cur.Add(37 * time.Minute)
	}
}

func TestDayOfMonthAndDayOfWeekAreOred(t *testing.T) {
	// "the first of the month, and every Monday". Vixie cron unions the two
	// restricted day fields. March 2026 starts on a Sunday, so the matches
	// are the 1st, then the Mondays: 2, 9, 16, 23, 30.
	s := mustParse(t, "0 0 1 3 1")
	var got []int
	cur := at("2026-02-28T23:59:00Z")
	for {
		next, err := s.Next(cur)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if next.Month() != time.March || next.Year() != 2026 {
			break
		}
		got = append(got, next.Day())
		cur = next
	}
	want := []int{1, 2, 9, 16, 23, 30}
	if len(got) != len(want) {
		t.Fatalf("matched days %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matched days %v, want %v", got, want)
		}
	}
}

func TestDayOfMonthAloneIsNotOred(t *testing.T) {
	// With day of week left as "*", only the day of month applies. The union
	// rule must not fire here, or a monthly backup would run every day.
	s := mustParse(t, "0 0 15 * *")
	next, err := s.Next(at("2026-03-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next.Day() != 15 {
		t.Fatalf("next day = %d, want 15", next.Day())
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"too few fields":         "0 2 * *",
		"too many fields":        "0 2 * * * *",
		"a macro":                "@daily",
		"a month name":           "0 0 1 JAN *",
		"a weekday name":         "0 0 * * MON",
		"minute out of range":    "60 2 * * *",
		"hour out of range":      "0 24 * * *",
		"day zero":               "0 0 0 * *",
		"month thirteen":         "0 0 1 13 *",
		"weekday eight":          "0 0 * * 8",
		"a backwards range":      "0 17-9 * * *",
		"a zero step":            "*/0 * * * *",
		"a negative step":        "*/-5 * * * *",
		"an empty list member":   "0,,30 * * * *",
		"a permanently dead day": "0 0 30 2 *",
		"nothing at all":         "",
	}
	for name, expr := range cases {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) accepted %s", expr, name)
		}
	}
}

func TestParseDeadScheduleReportsNoSlot(t *testing.T) {
	_, err := Parse("0 0 31 2 *")
	if !errors.Is(err, ErrNoSlot) {
		t.Fatalf("Parse(31 Feb) returned %v, want it to wrap ErrNoSlot", err)
	}
}

func TestParseNormalisesWhitespace(t *testing.T) {
	s := mustParse(t, "  0\t2  *   * *  ")
	if s.String() != "0 2 * * *" {
		t.Fatalf("String() = %q, want the collapsed expression", s.String())
	}
}

func TestCountBetween(t *testing.T) {
	s := mustParse(t, "*/10 * * * *")
	n, truncated := s.CountBetween(at("2026-03-01T00:00:00Z"), at("2026-03-01T01:00:00Z"), 100)
	if n != 6 || truncated {
		t.Fatalf("CountBetween over an hour = (%d, %v), want (6, false)", n, truncated)
	}
	n, truncated = s.CountBetween(at("2026-03-01T00:00:00Z"), at("2026-03-15T00:00:00Z"), 50)
	if n != 50 || !truncated {
		t.Fatalf("CountBetween over a fortnight = (%d, %v), want (50, true)", n, truncated)
	}
	n, truncated = s.CountBetween(at("2026-03-01T00:00:00Z"), at("2026-03-01T00:00:00Z"), 100)
	if n != 0 || truncated {
		t.Fatalf("CountBetween over an empty interval = (%d, %v), want (0, false)", n, truncated)
	}
}

func TestStepOnBareNumberRunsToTopOfField(t *testing.T) {
	// "5/15" means 5, 20, 35, 50. It is not a single value with a step that
	// goes nowhere.
	s := mustParse(t, "5/15 * * * *")
	var mins []int
	cur := at("2026-03-01T00:00:00Z")
	for i := 0; i < 4; i++ {
		next, err := s.Next(cur)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		mins = append(mins, next.Minute())
		cur = next
	}
	want := []int{5, 20, 35, 50}
	for i := range want {
		if mins[i] != want[i] {
			t.Fatalf("minutes = %v, want %v", mins, want)
		}
	}
}

func BenchmarkNextOnceAYear(b *testing.B) {
	s, err := Parse("0 0 1 1 *")
	if err != nil {
		b.Fatal(err)
	}
	from := at("2026-01-02T00:00:00Z")
	for i := 0; i < b.N; i++ {
		if _, err := s.Next(from); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNextEveryFiveMinutes(b *testing.B) {
	s, err := Parse("*/5 * * * *")
	if err != nil {
		b.Fatal(err)
	}
	from := at("2026-01-02T00:01:00Z")
	for i := 0; i < b.N; i++ {
		if _, err := s.Next(from); err != nil {
			b.Fatal(err)
		}
	}
}
