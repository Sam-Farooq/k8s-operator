package controller

import (
	"testing"
	"time"
)

func TestRequeueInIsClamped(t *testing.T) {
	now := slot("2026-10-08T02:30:00Z")
	cases := []struct {
		name string
		next time.Time
		want time.Duration
	}{
		{"a slot in the past", now.Add(-time.Hour), requeueFloor},
		{"a slot now", now, requeueFloor},
		{"a slot in 30 seconds", now.Add(30 * time.Second), 31 * time.Second},
		{"a slot tomorrow", now.Add(24 * time.Hour), requeueCeiling},
	}
	for _, c := range cases {
		if got := requeueIn(now, c.next); got != c.want {
			t.Errorf("%s: requeueIn = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestRequeueInLandsAfterTheSlot(t *testing.T) {
	// Waking exactly on the slot is a race with the clock's own rounding, and
	// losing it means the slot is not seen until the next requeue.
	now := slot("2026-10-08T02:00:00Z")
	next := now.Add(5 * time.Minute)
	if got := now.Add(requeueIn(now, next)); !got.After(next) {
		t.Fatalf("the requeue lands at %s, which is not after the slot at %s", got, next)
	}
}

func TestCleanupBackoffGrowsAndStops(t *testing.T) {
	if got := cleanupBackoff(0); got != time.Second {
		t.Errorf("cleanupBackoff(0) = %s, want 1s", got)
	}
	prev := time.Duration(0)
	for _, elapsed := range []time.Duration{0, time.Second, 5 * time.Second, 20 * time.Second, time.Minute, 10 * time.Minute, time.Hour} {
		got := cleanupBackoff(elapsed)
		if got < prev {
			t.Errorf("cleanupBackoff(%s) = %s went backwards from %s", elapsed, got, prev)
		}
		if got > cleanupBackoffCeiling {
			t.Errorf("cleanupBackoff(%s) = %s is past the ceiling", elapsed, got)
		}
		prev = got
	}
	if got := cleanupBackoff(time.Hour); got != cleanupBackoffCeiling {
		t.Errorf("cleanupBackoff(1h) = %s, want the ceiling", got)
	}
}
