package controller

import (
	"fmt"
	"time"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
	"github.com/Sam-Farooq/k8s-operator/internal/schedule"
)

// missedSlotCap bounds the count reported in a log line and in
// status.skippedSlots. An operator restarted after a fortnight on a five
// minute schedule has 4,032 slots behind it and the exact figure buys
// nothing.
const missedSlotCap = 1000

// action is what a reconcile should do about the current slot.
type action int

const (
	// actionNone means no slot has fallen since the last one handled.
	actionNone action = iota
	// actionAdopt means the slot already has its Job. This is the common
	// case on a second reconcile and it is deliberately not a create.
	actionAdopt
	// actionCreate means the slot is due and nothing is running it.
	actionCreate
	// actionSkip means the slot is due and will not be run.
	actionSkip
)

func (a action) String() string {
	switch a {
	case actionAdopt:
		return "adopt"
	case actionCreate:
		return "create"
	case actionSkip:
		return "skip"
	default:
		return "none"
	}
}

// decision is the whole of what a reconcile decided, as data. Keeping it
// separate from the acting means the hard part, which slot and why, is
// testable without an API server.
type decision struct {
	act     action
	slot    time.Time
	advance bool
	skipped int32
	reason  string
	message string

	// collision is set by the acting half when the Job name for the slot is
	// taken by something this object does not control.
	collision string
}

// decide picks the slot to act on and what to do with it.
//
// Only the most recent slot is ever considered. A controller that walks every
// slot it missed creates a Job for each one, and the first restart after a
// weekend outage then runs a fortnight of backups at once against a live
// database. The slots that went by are counted, not run.
func decide(pb *opsv1alpha1.PostgresBackup, sched schedule.Schedule, obs observation, now time.Time) decision {
	// The floor is the last slot acted on, or the object's creation. Using
	// creation rather than the zero time is what stops a new PostgresBackup
	// with a yearly schedule from immediately running the slot from January.
	floor := pb.CreationTimestamp.Time.UTC()
	if pb.Status.LastScheduleTime != nil {
		floor = pb.Status.LastScheduleTime.Time.UTC()
	}

	latest, err := sched.Latest(now)
	if err != nil || !latest.After(floor) {
		return decision{act: actionNone}
	}

	missed, truncated := sched.CountBetween(floor, now, missedSlotCap)
	d := decision{slot: latest, advance: true}

	// The Job for this slot already exists. Its name is derived from the
	// slot, so this is also what makes a repeated Reconcile a no-op.
	if obs.bySlot[SlotLabel(latest)] != nil {
		d.act = actionAdopt
		d.skipped = behind(missed, truncated)
		return d
	}

	lateBy := now.Sub(latest)
	deadline := time.Duration(pb.Spec.StartingDeadlineSeconds) * time.Second

	switch {
	case pb.Spec.Suspend:
		d.act = actionSkip
		d.reason = opsv1alpha1.ReasonSuspended
		d.message = fmt.Sprintf("spec.suspend is true, so the slot at %s was not run", latest.Format(time.RFC3339))
		d.skipped = int32(missed)

	case lateBy > deadline:
		d.act = actionSkip
		d.reason = opsv1alpha1.ReasonDeadlineExceeded
		d.message = fmt.Sprintf("the slot at %s is %s late, past spec.startingDeadlineSeconds of %s",
			latest.Format(time.RFC3339), lateBy.Round(time.Second), deadline)
		d.skipped = int32(missed)

	case pb.Spec.ConcurrencyPolicy == opsv1alpha1.ConcurrencyForbid && len(obs.active) > 0:
		d.act = actionSkip
		d.reason = opsv1alpha1.ReasonConcurrencyForbidden
		d.message = fmt.Sprintf("%s is still running and concurrencyPolicy is Forbid, so the slot at %s was not run",
			obs.active[0].job.Name, latest.Format(time.RFC3339))
		d.skipped = int32(missed)

	default:
		d.act = actionCreate
		d.skipped = behind(missed, truncated)
	}
	return d
}

// behind is how many slots other than the one being acted on went by. It is
// what makes status.skippedSlots count an outage rather than only the slot
// that was still inside its deadline when the operator came back.
func behind(missed int, truncated bool) int32 {
	if missed <= 1 && !truncated {
		return 0
	}
	return int32(missed - 1)
}
