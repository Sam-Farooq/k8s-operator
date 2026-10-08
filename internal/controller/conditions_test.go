package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

func conditionsFor(pb *opsv1alpha1.PostgresBackup, obs observation, d decision) *opsv1alpha1.PostgresBackupStatus {
	status := pb.Status.DeepCopy()
	setConditions(status, pb, obs, d)
	return status
}

func cond(status *opsv1alpha1.PostgresBackupStatus, name string) *metav1.Condition {
	return meta.FindStatusCondition(status.Conditions, name)
}

func TestConditionsAllThreeArePresentFromTheStart(t *testing.T) {
	pb := sample()
	pb.Generation = 4
	status := conditionsFor(pb, observe(nil), decision{act: actionNone})

	for _, name := range []string{opsv1alpha1.ConditionReady, opsv1alpha1.ConditionRunning, opsv1alpha1.ConditionDegraded} {
		c := cond(status, name)
		if c == nil {
			t.Fatalf("%s is absent; a condition that comes and goes cannot be alerted on", name)
		}
		if c.ObservedGeneration != 4 {
			t.Errorf("%s carries observedGeneration %d, want 4", name, c.ObservedGeneration)
		}
		if c.Reason == "" {
			t.Errorf("%s has no reason, which the API server rejects", name)
		}
	}
}

func TestConditionsNoRunsYet(t *testing.T) {
	status := conditionsFor(sample(), observe(nil), decision{act: actionNone})
	if got := cond(status, opsv1alpha1.ConditionReady); got.Status != metav1.ConditionUnknown {
		t.Errorf("Ready = %s with nothing having run, want Unknown", got.Status)
	}
	if got := cond(status, opsv1alpha1.ConditionDegraded); got.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %s with nothing having run, want False", got.Status)
	}
}

func TestConditionsLastRunSucceeded(t *testing.T) {
	obs := observe([]batchv1.Job{
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "succeeded"),
		jobAt("db-20261007-0230", "2026-10-07T02:30:00Z", "failed"),
	})
	status := conditionsFor(sample(), obs, decision{act: actionNone})
	if got := cond(status, opsv1alpha1.ConditionReady); got.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s, want True: the newest finished run succeeded", got.Status)
	}
	if got := cond(status, opsv1alpha1.ConditionDegraded); got.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %s, want False: an older failure is history, not a problem", got.Status)
	}
}

func TestConditionsLastRunFailed(t *testing.T) {
	obs := observe([]batchv1.Job{
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "failed"),
		jobAt("db-20261007-0230", "2026-10-07T02:30:00Z", "succeeded"),
	})
	status := conditionsFor(sample(), obs, decision{act: actionNone})
	ready := cond(status, opsv1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != opsv1alpha1.ReasonLastRunFailed {
		t.Errorf("Ready = %s/%s, want False/LastRunFailed", ready.Status, ready.Reason)
	}
	degraded := cond(status, opsv1alpha1.ConditionDegraded)
	if degraded.Status != metav1.ConditionTrue {
		t.Errorf("Degraded = %s, want True", degraded.Status)
	}
	if degraded.Message == "" {
		t.Error("Degraded did not say which Job failed")
	}
}

func TestConditionsARunningJobDoesNotHideTheLastResult(t *testing.T) {
	// The newest run is in flight; Ready has to describe the newest FINISHED
	// one, or a nightly failure is invisible for as long as tonight's dump
	// takes.
	obs := observe([]batchv1.Job{
		jobAt("db-20261009-0230", "2026-10-09T02:30:00Z", "running"),
		jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "failed"),
	})
	status := conditionsFor(sample(), obs, decision{act: actionNone})
	if got := cond(status, opsv1alpha1.ConditionRunning); got.Status != metav1.ConditionTrue {
		t.Errorf("Running = %s, want True", got.Status)
	}
	if got := cond(status, opsv1alpha1.ConditionReady); got.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %s, want False from the failure underneath the running Job", got.Status)
	}
}

func TestConditionsRunningNamesTheJobs(t *testing.T) {
	obs := observe([]batchv1.Job{
		jobAt("db-20261009-0230", "2026-10-09T02:30:00Z", "running"),
		jobAt("db-20261009-0330", "2026-10-09T03:30:00Z", "running"),
	})
	c := cond(conditionsFor(sample(), obs, decision{act: actionNone}), opsv1alpha1.ConditionRunning)
	if c.Message != "2 Jobs are running: db-20261009-0330, db-20261009-0230" {
		t.Errorf("Running message = %q", c.Message)
	}
}

func TestConditionsSuspended(t *testing.T) {
	pb := sample()
	pb.Spec.Suspend = true
	obs := observe([]batchv1.Job{jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "succeeded")})
	status := conditionsFor(pb, obs, decision{act: actionSkip, reason: opsv1alpha1.ReasonSuspended})

	ready := cond(status, opsv1alpha1.ConditionReady)
	if ready.Status != metav1.ConditionFalse || ready.Reason != opsv1alpha1.ReasonSuspended {
		t.Errorf("Ready = %s/%s, want False/Suspended even though the last run succeeded", ready.Status, ready.Reason)
	}
	if got := cond(status, opsv1alpha1.ConditionDegraded); got.Status != metav1.ConditionFalse {
		t.Errorf("Degraded = %s, want False: suspension is intentional", got.Status)
	}
}

func TestConditionsCollisionOutranksEverything(t *testing.T) {
	obs := observe([]batchv1.Job{jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "succeeded")})
	status := conditionsFor(sample(), obs, decision{act: actionCreate, collision: "orders-nightly-20261009-0230"})
	degraded := cond(status, opsv1alpha1.ConditionDegraded)
	if degraded.Status != metav1.ConditionTrue || degraded.Reason != opsv1alpha1.ReasonJobNameCollision {
		t.Fatalf("Degraded = %s/%s, want True/JobNameCollision", degraded.Status, degraded.Reason)
	}
	if got := cond(status, opsv1alpha1.ConditionReady); got.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %s; the last run still succeeded, so Ready is not the field that reports this", got.Status)
	}
}

func TestConditionsSkippedSlotIsDegraded(t *testing.T) {
	d := decision{act: actionSkip, reason: opsv1alpha1.ReasonDeadlineExceeded, message: "11m late"}
	status := conditionsFor(sample(), observe(nil), d)
	degraded := cond(status, opsv1alpha1.ConditionDegraded)
	if degraded.Status != metav1.ConditionTrue || degraded.Reason != opsv1alpha1.ReasonDeadlineExceeded {
		t.Fatalf("Degraded = %s/%s", degraded.Status, degraded.Reason)
	}
	if degraded.Message != "11m late" {
		t.Errorf("Degraded message = %q, want the decision's own message", degraded.Message)
	}
}

func TestConditionsReasonsAreTokens(t *testing.T) {
	// The API server rejects a reason with a space or punctuation in it, and
	// the only way that shows up is a status write failing in production.
	obs := observe([]batchv1.Job{jobAt("db-20261008-0230", "2026-10-08T02:30:00Z", "failed")})
	status := conditionsFor(sample(), obs, decision{act: actionNone})
	for _, c := range status.Conditions {
		for _, r := range c.Reason {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
				t.Errorf("condition %s has reason %q, which is not a CamelCase token", c.Type, c.Reason)
				break
			}
		}
	}
}
