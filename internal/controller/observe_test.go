package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// jobAt builds a Job the way the controller would, then applies a phase to
// it. phase is "running", "succeeded" or "failed".
func jobAt(name, slotRFC, phase string) batchv1.Job {
	s := slot(slotRFC)
	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "data",
			CreationTimestamp: metav1.NewTime(s),
			Labels:            map[string]string{opsv1alpha1.LabelSlot: SlotLabel(s)},
		},
	}
	start := metav1.NewTime(s.Add(2 * time.Second))
	job.Status.StartTime = &start
	switch phase {
	case "succeeded":
		done := metav1.NewTime(s.Add(90 * time.Second))
		job.Status.CompletionTime = &done
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached"},
		}
	case "failed":
		job.Status.Failed = 3
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}
	}
	return job
}

func TestObserveClassifiesAndOrders(t *testing.T) {
	jobs := []batchv1.Job{
		jobAt("db-20261006-0230", "2026-10-06T02:30:00Z", "succeeded"),
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "running"),
		jobAt("db-20261007-0230", "2026-10-07T02:30:00Z", "failed"),
	}
	obs := observe(jobs)

	if len(obs.runs) != 3 {
		t.Fatalf("observed %d runs, want 3", len(obs.runs))
	}
	if obs.runs[0].job.Name != "db-20261008-0230" {
		t.Errorf("newest run is %q, want the 8th", obs.runs[0].job.Name)
	}
	if obs.runs[2].job.Name != "db-20261006-0230" {
		t.Errorf("oldest run is %q, want the 6th", obs.runs[2].job.Name)
	}
	if len(obs.active) != 1 || len(obs.succeeded) != 1 || len(obs.failed) != 1 {
		t.Fatalf("active=%d succeeded=%d failed=%d", len(obs.active), len(obs.succeeded), len(obs.failed))
	}
	if obs.failed[0].reason != "BackoffLimitExceeded" {
		t.Errorf("failure reason = %q", obs.failed[0].reason)
	}
}

func TestObserveFindsTheJobForASlot(t *testing.T) {
	s := slot("2026-10-08T02:30:00Z")
	obs := observe([]batchv1.Job{jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "running")})
	if obs.bySlot[SlotLabel(s)] == nil {
		t.Fatal("the Job for the slot was not found by its label")
	}
	if obs.bySlot[SlotLabel(s.Add(24*time.Hour))] != nil {
		t.Fatal("a slot with no Job returned one")
	}
}

func TestObserveRetryingJobIsStillRunning(t *testing.T) {
	// job.Status.Failed is a pod counter. A Job retrying inside its
	// backoffLimit has a non-zero Failed and no Failed condition, and calling
	// that a failure would mark the backup failed while it is still working.
	job := jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "running")
	job.Status.Failed = 1
	obs := observe([]batchv1.Job{job})
	if len(obs.active) != 1 {
		t.Fatalf("a retrying Job was classified as %s", obs.runs[0].phase)
	}
}

func TestObserveFalseConditionsAreIgnored(t *testing.T) {
	job := jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "running")
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
		{Type: batchv1.JobFailed, Status: corev1.ConditionFalse},
	}
	obs := observe([]batchv1.Job{job})
	if obs.runs[0].phase != opsv1alpha1.RunRunning {
		t.Fatalf("phase = %s, want Running: a condition at False says the opposite of True", obs.runs[0].phase)
	}
}

func TestObserveFallsBackToCreationTimestamp(t *testing.T) {
	job := jobAt("hand-made", "2026-10-08T02:30:00Z", "running")
	delete(job.Labels, opsv1alpha1.LabelSlot)
	obs := observe([]batchv1.Job{job})
	if len(obs.active) != 1 {
		t.Fatal("a Job with no slot label was dropped, which would leak it")
	}
	if !obs.runs[0].slot.Equal(slot("2026-10-08T02:30:00Z")) {
		t.Errorf("slot = %s, want the creation timestamp", obs.runs[0].slot)
	}
}

func TestObserveUnparseableLabelFallsBack(t *testing.T) {
	job := jobAt("db", "2026-10-08T02:30:00Z", "running")
	job.Labels[opsv1alpha1.LabelSlot] = "yesterday"
	obs := observe([]batchv1.Job{job})
	if !obs.runs[0].slot.Equal(slot("2026-10-08T02:30:00Z")) {
		t.Errorf("slot = %s, want the creation timestamp", obs.runs[0].slot)
	}
}

func TestHistoryDropsCompletionTimeOnFailure(t *testing.T) {
	failed := jobAt("db-20261007-0230", "2026-10-07T02:30:00Z", "failed")
	done := metav1.NewTime(slot("2026-10-07T02:31:00Z"))
	failed.Status.CompletionTime = &done // a Job should not have this; assert we ignore it
	h := observe([]batchv1.Job{failed}).history()
	if len(h) != 1 {
		t.Fatalf("history has %d records", len(h))
	}
	if h[0].CompletionTime != nil {
		t.Error("a failed run reported a completion time")
	}
	if h[0].StartTime == nil {
		t.Error("a failed run lost its start time")
	}
}

func TestHistoryIsNilWhenNothingHasRun(t *testing.T) {
	// nil rather than an empty slice, so the status subresource does not
	// churn between absent and [] on every reconcile.
	if h := observe(nil).history(); h != nil {
		t.Errorf("history on an empty observation = %v, want nil", h)
	}
	if n := observe(nil).activeNames(); n != nil {
		t.Errorf("activeNames on an empty observation = %v, want nil", n)
	}
	if ls := observe(nil).lastSuccessful(); ls != nil {
		t.Errorf("lastSuccessful on an empty observation = %v, want nil", ls)
	}
}

func TestLastSuccessfulPicksTheNewestCompletion(t *testing.T) {
	obs := observe([]batchv1.Job{
		jobAt("db-20261006-0230", "2026-10-06T02:30:00Z", "succeeded"),
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "succeeded"),
		jobAt("db-20261007-0230", "2026-10-07T02:30:00Z", "failed"),
	})
	got := obs.lastSuccessful()
	if got == nil {
		t.Fatal("lastSuccessful is nil with two successes present")
	}
	want := slot("2026-10-08T02:30:00Z").Add(90 * time.Second)
	if !got.Time.Equal(want) {
		t.Errorf("lastSuccessful = %s, want %s", got.Time, want)
	}
}

func TestPrunableKeepsTheNewest(t *testing.T) {
	var jobs []batchv1.Job
	for day := 1; day <= 5; day++ {
		jobs = append(jobs, jobAt(
			"db-2026100"+string(rune('0'+day))+"-0230",
			"2026-10-0"+string(rune('0'+day))+"T02:30:00Z",
			"succeeded",
		))
	}
	jobs = append(jobs,
		jobAt("db-20261001-0330", "2026-10-01T03:30:00Z", "failed"),
		jobAt("db-20261002-0330", "2026-10-02T03:30:00Z", "failed"),
		jobAt("db-20261003-0330", "2026-10-03T03:30:00Z", "failed"),
		jobAt("db-20261009-0230", "2026-10-09T02:30:00Z", "running"),
	)
	obs := observe(jobs)

	prunable := obs.prunable(2, 1)
	names := map[string]bool{}
	for _, j := range prunable {
		names[j.Name] = true
	}
	if len(prunable) != 5 {
		t.Fatalf("prunable = %v, want 3 successes and 2 failures", names)
	}
	for _, keep := range []string{"db-20261005-0230", "db-20261004-0230", "db-20261003-0330", "db-20261009-0230"} {
		if names[keep] {
			t.Errorf("%s was marked prunable but is inside the retention window", keep)
		}
	}
	for _, drop := range []string{"db-20261003-0230", "db-20261002-0230", "db-20261001-0230", "db-20261002-0330", "db-20261001-0330"} {
		if !names[drop] {
			t.Errorf("%s is outside the retention window and was not marked prunable", drop)
		}
	}
}

func TestPrunableNeverTouchesAnActiveJob(t *testing.T) {
	obs := observe([]batchv1.Job{
		jobAt("db-20261009-0230", "2026-10-09T02:30:00Z", "running"),
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "running"),
	})
	if got := obs.prunable(0, 0); len(got) != 0 {
		t.Fatalf("prunable with keep 0/0 returned %d running Jobs", len(got))
	}
}
