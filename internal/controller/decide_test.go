package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
	"github.com/Sam-Farooq/k8s-operator/internal/schedule"
)

// nightly is the fixture schedule: 02:30 every day, UTC.
func nightly(t *testing.T) schedule.Schedule {
	t.Helper()
	s, err := schedule.Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func backupCreatedAt(rfc string) *opsv1alpha1.PostgresBackup {
	pb := sample()
	pb.CreationTimestamp = metav1.NewTime(slot(rfc))
	return pb
}

func TestDecideNoSlotYet(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T02:31:00Z")
	d := decide(pb, nightly(t), observe(nil), slot("2026-10-08T06:00:00Z"))
	if d.act != actionNone {
		t.Fatalf("act = %s, want none: the only slot today fell before the object existed", d.act)
	}
	if d.advance {
		t.Error("lastScheduleTime would have moved with no slot to move it to")
	}
}

func TestDecideCreatesTheDueSlot(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	d := decide(pb, nightly(t), observe(nil), slot("2026-10-08T02:30:20Z"))
	if d.act != actionCreate {
		t.Fatalf("act = %s, want create", d.act)
	}
	if !d.slot.Equal(slot("2026-10-08T02:30:00Z")) {
		t.Errorf("slot = %s", d.slot)
	}
	if !d.advance || d.skipped != 0 {
		t.Errorf("advance = %v, skipped = %d, want true and 0", d.advance, d.skipped)
	}
}

func TestDecideAdoptsAnExistingJob(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	obs := observe([]batchv1.Job{jobAt("orders-nightly-20261008-0230", "2026-10-08T02:30:00Z", "running")})
	d := decide(pb, nightly(t), obs, slot("2026-10-08T02:31:00Z"))
	if d.act != actionAdopt {
		t.Fatalf("act = %s, want adopt: the Job for the slot is already there", d.act)
	}
	if !d.advance {
		t.Error("an adopted slot should still move lastScheduleTime, or it is reconsidered forever")
	}
}

func TestDecideStopsOnceTheSlotIsRecorded(t *testing.T) {
	// The second reconcile of a slot has to be a no-op, which is what makes
	// Reconcile idempotent. lastScheduleTime is the floor.
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	handled := metav1.NewTime(slot("2026-10-08T02:30:00Z"))
	pb.Status.LastScheduleTime = &handled
	obs := observe([]batchv1.Job{jobAt("orders-nightly-20261008-0230", "2026-10-08T02:30:00Z", "running")})
	d := decide(pb, nightly(t), obs, slot("2026-10-08T02:31:00Z"))
	if d.act != actionNone {
		t.Fatalf("act = %s, want none", d.act)
	}
}

func TestDecideSuspended(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	pb.Spec.Suspend = true
	d := decide(pb, nightly(t), observe(nil), slot("2026-10-08T02:30:10Z"))
	if d.act != actionSkip || d.reason != opsv1alpha1.ReasonSuspended {
		t.Fatalf("act = %s, reason = %q", d.act, d.reason)
	}
	if d.skipped != 1 {
		t.Errorf("skipped = %d, want 1: a suspended slot is a slot that did not run", d.skipped)
	}
	if !d.advance {
		t.Error("a suspended slot must still move the floor, or resuming runs a slot from last week")
	}
}

func TestDecideStartingDeadline(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	pb.Spec.StartingDeadlineSeconds = 600

	inside := decide(pb, nightly(t), observe(nil), slot("2026-10-08T02:39:00Z"))
	if inside.act != actionCreate {
		t.Fatalf("nine minutes late: act = %s, want create", inside.act)
	}

	outside := decide(pb, nightly(t), observe(nil), slot("2026-10-08T02:41:00Z"))
	if outside.act != actionSkip || outside.reason != opsv1alpha1.ReasonDeadlineExceeded {
		t.Fatalf("eleven minutes late: act = %s, reason = %q", outside.act, outside.reason)
	}
	if outside.message == "" {
		t.Error("a skipped slot with no message gives an operator nothing to read")
	}
}

func TestDecideOnlyEverConsidersTheLatestSlot(t *testing.T) {
	// Eight days down, hourly schedule. A controller that walked the missed
	// slots would create 192 Jobs on the first reconcile after it came back.
	hourly, err := schedule.Parse("0 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	pb := backupCreatedAt("2026-09-30T00:00:00Z")
	handled := metav1.NewTime(slot("2026-09-30T00:00:00Z"))
	pb.Status.LastScheduleTime = &handled

	d := decide(pb, hourly, observe(nil), slot("2026-10-08T03:05:00Z"))
	if d.act != actionCreate {
		t.Fatalf("act = %s, want create", d.act)
	}
	if !d.slot.Equal(slot("2026-10-08T03:00:00Z")) {
		t.Errorf("slot = %s, want the most recent one", d.slot)
	}
	// 2026-09-30T01:00 through 2026-10-08T03:00 inclusive is 195 slots, of
	// which 194 are not the one being run.
	if d.skipped != 194 {
		t.Errorf("skipped = %d, want 194", d.skipped)
	}
}

func TestDecideSkippedCountIsCapped(t *testing.T) {
	everyMinute, err := schedule.Parse("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	pb := backupCreatedAt("2026-01-01T00:00:00Z")
	handled := metav1.NewTime(slot("2026-01-01T00:00:00Z"))
	pb.Status.LastScheduleTime = &handled

	d := decide(pb, everyMinute, observe(nil), slot("2026-10-08T03:05:00Z"))
	if d.skipped != missedSlotCap-1 {
		t.Fatalf("skipped = %d, want the cap minus the slot being acted on (%d)", d.skipped, missedSlotCap-1)
	}
}

func TestDecideConcurrencyForbid(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	pb.Spec.ConcurrencyPolicy = opsv1alpha1.ConcurrencyForbid
	obs := observe([]batchv1.Job{jobAt("orders-nightly-20261007-0230", "2026-10-07T02:30:00Z", "running")})

	d := decide(pb, nightly(t), obs, slot("2026-10-08T02:30:10Z"))
	if d.act != actionSkip || d.reason != opsv1alpha1.ReasonConcurrencyForbidden {
		t.Fatalf("act = %s, reason = %q", d.act, d.reason)
	}
	if d.message == "" {
		t.Error("the skip did not name the Job that blocked it")
	}
}

func TestDecideConcurrencyAllow(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	pb.Spec.ConcurrencyPolicy = opsv1alpha1.ConcurrencyAllow
	obs := observe([]batchv1.Job{jobAt("orders-nightly-20261007-0230", "2026-10-07T02:30:00Z", "running")})

	d := decide(pb, nightly(t), obs, slot("2026-10-08T02:30:10Z"))
	if d.act != actionCreate {
		t.Fatalf("act = %s, want create: Allow means the running Job does not block the slot", d.act)
	}
}

func TestDecideAFinishedJobDoesNotBlockTheNextSlot(t *testing.T) {
	pb := backupCreatedAt("2026-10-08T00:00:00Z")
	obs := observe([]batchv1.Job{
		jobAt("orders-nightly-20261007-0230", "2026-10-07T02:30:00Z", "succeeded"),
		jobAt("orders-nightly-20261006-0230", "2026-10-06T02:30:00Z", "failed"),
	})
	d := decide(pb, nightly(t), obs, slot("2026-10-08T02:30:10Z"))
	if d.act != actionCreate {
		t.Fatalf("act = %s, want create", d.act)
	}
}
