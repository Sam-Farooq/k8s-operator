package controller

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// The three conditions follow the Kubernetes convention. Ready and Running
// are normal-true: they are present whatever the state. Degraded is
// abnormal-true: it is still always present, because a condition that appears
// and disappears is a condition nobody can write an alert against, but it is
// true only when something wants attention.

func runningStatus(obs observation) metav1.ConditionStatus {
	if len(obs.active) > 0 {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func runningReason(obs observation) string {
	if len(obs.active) > 0 {
		return opsv1alpha1.ReasonJobRunning
	}
	return opsv1alpha1.ReasonNoJobRunning
}

func runningMessage(obs observation) string {
	switch n := len(obs.active); n {
	case 0:
		return "No backup Job is running"
	case 1:
		return fmt.Sprintf("Job %s is running", obs.active[0].job.Name)
	default:
		names := make([]string, 0, n)
		for _, r := range obs.active {
			names = append(names, r.job.Name)
		}
		return fmt.Sprintf("%d Jobs are running: %s", n, strings.Join(names, ", "))
	}
}

// newestFinished is the most recent run that is neither running nor pending.
// obs.runs is newest slot first, so the first non-running entry is it.
func newestFinished(obs observation) *run {
	for i := range obs.runs {
		if obs.runs[i].phase != opsv1alpha1.RunRunning {
			return &obs.runs[i]
		}
	}
	return nil
}

// setConditions writes all three conditions from one place, so two code paths
// cannot each set Degraded and have the later one win silently.
func setConditions(status *opsv1alpha1.PostgresBackupStatus, pb *opsv1alpha1.PostgresBackup, obs observation, d decision) {
	gen := pb.Generation

	setCondition(status, gen, opsv1alpha1.ConditionRunning,
		runningStatus(obs), runningReason(obs), runningMessage(obs))

	last := newestFinished(obs)

	// Ready is what a person reads first, so it answers "is this database
	// being backed up". A suspended PostgresBackup is not, and says so. That
	// is a choice: the suspension is intentional, so Degraded stays false,
	// but a Ready that reported true while nothing was scheduled would be
	// the field that makes an incident last longer.
	switch {
	case pb.Spec.Suspend:
		setCondition(status, gen, opsv1alpha1.ConditionReady, metav1.ConditionFalse,
			opsv1alpha1.ReasonSuspended, "spec.suspend is true, so no new Job will be created")
	case last == nil:
		setCondition(status, gen, opsv1alpha1.ConditionReady, metav1.ConditionUnknown,
			opsv1alpha1.ReasonNoRunsYet, readyNoRunsMessage(status))
	case last.phase == opsv1alpha1.RunSucceeded:
		setCondition(status, gen, opsv1alpha1.ConditionReady, metav1.ConditionTrue,
			opsv1alpha1.ReasonLastRunSucceeded,
			fmt.Sprintf("Job %s completed the slot at %s", last.job.Name, last.slot.Format(time.RFC3339)))
	default:
		setCondition(status, gen, opsv1alpha1.ConditionReady, metav1.ConditionFalse,
			opsv1alpha1.ReasonLastRunFailed,
			fmt.Sprintf("Job %s failed the slot at %s: %s", last.job.Name, last.slot.Format(time.RFC3339), orNone(last.reason)))
	}

	// A skipped slot is not in here. It is an Event and a tick of
	// status.skippedSlots, because the skip advances lastScheduleTime, so the
	// next reconcile a second later has no slot to decide about and would set
	// Degraded back to False. A condition that is true for one reconcile is a
	// condition nobody can write an alert against.
	//
	// A name collision is in here, and the difference is that it is sticky by
	// construction: lastScheduleTime is deliberately left where it was, so
	// every following reconcile reaches the same conclusion about the same
	// slot until somebody changes something.
	switch {
	case d.collision != "":
		setCondition(status, gen, opsv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			opsv1alpha1.ReasonJobNameCollision,
			fmt.Sprintf("Job %s exists and is not controlled by this object; rename this PostgresBackup or delete that Job", d.collision))
	case last != nil && last.phase == opsv1alpha1.RunFailed:
		setCondition(status, gen, opsv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			opsv1alpha1.ReasonLastRunFailed,
			fmt.Sprintf("the newest finished run, %s, failed: %s", last.job.Name, orNone(last.reason)))
	default:
		setCondition(status, gen, opsv1alpha1.ConditionDegraded, metav1.ConditionFalse,
			opsv1alpha1.ReasonHealthy, "No condition needs attention")
	}
}

func readyNoRunsMessage(status *opsv1alpha1.PostgresBackupStatus) string {
	if status.NextScheduleTime == nil {
		return "No run has finished yet"
	}
	return fmt.Sprintf("No run has finished yet; the next slot is %s", status.NextScheduleTime.Time.UTC().Format(time.RFC3339))
}

func orNone(s string) string {
	if s == "" {
		return "no reason reported"
	}
	return s
}

// setCondition sets one condition with the generation it was computed from. A
// status whose conditions carry an observedGeneration behind
// metadata.generation is a stale status, and that is the only way a reader
// can tell.
func setCondition(status *opsv1alpha1.PostgresBackupStatus, generation int64, condType string, state metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             state,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
