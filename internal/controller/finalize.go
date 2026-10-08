package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// cleanupBackoffCeiling caps the wait between cleanup attempts.
const cleanupBackoffCeiling = 30 * time.Second

// finalize runs while deletionTimestamp is set.
//
// The owner reference already makes the garbage collector delete these Jobs,
// so the finalizer is not there to free objects. It is there so that
// `kubectl delete postgresbackup` does not return while a pg_dump is still
// writing into the destination claim. Garbage collection is asynchronous: the
// object disappears first and the pod carries on.
//
// The other half is the case that makes finalizers notorious. A pod can sit
// Terminating indefinitely, and a volume on an unresponsive backing store is
// the usual way it happens for exactly this workload. Nothing the operator
// can do from here fixes that, so after spec.cleanupTimeoutSeconds it gives
// up, says in an event which Jobs it left behind, and removes the finalizer.
// An object that cannot be deleted is worse than a Job that outlives it.
func (r *PostgresBackupReconciler) finalize(ctx context.Context, pb *opsv1alpha1.PostgresBackup, obs observation) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(pb, opsv1alpha1.Finalizer) {
		// Someone else, or an earlier reconcile, already finished. The API
		// server will remove the object.
		return ctrl.Result{}, nil
	}

	if len(obs.runs) == 0 {
		return ctrl.Result{}, r.releaseFinalizer(ctx, pb)
	}

	now := r.Clock.Now().UTC()
	elapsed := now.Sub(pb.DeletionTimestamp.UTC())
	budget := pb.Spec.CleanupTimeout()

	remaining := make([]string, 0, len(obs.runs))
	for i := range obs.runs {
		job := obs.runs[i].job
		remaining = append(remaining, job.Name)
		if !job.DeletionTimestamp.IsZero() {
			continue
		}
		err := r.Delete(ctx, job,
			client.PropagationPolicy(metav1.DeletePropagationBackground),
			client.Preconditions{UID: &job.UID},
		)
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			// Keep going rather than returning. One Job that will not delete
			// must not stop the others being asked, and the deadline below is
			// what bounds the whole thing.
			logger.Error(err, "asking for an owned Job to be deleted", "job", job.Name)
		}
	}

	if elapsed < budget {
		status := pb.Status.DeepCopy()
		setCondition(status, pb.Generation, opsv1alpha1.ConditionReady, metav1.ConditionFalse,
			opsv1alpha1.ReasonCleanupInProgress,
			fmt.Sprintf("waiting for %d owned Job(s) to go away: %s", len(remaining), strings.Join(remaining, ", ")))
		if err := r.writeStatus(ctx, pb, status); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// A Job stuck Terminating produces no further watch events, so the
		// timer is the only thing that brings us back. It widens so a volume
		// that is slow rather than dead is not hammered.
		return ctrl.Result{RequeueAfter: cleanupBackoff(elapsed)}, nil
	}

	message := fmt.Sprintf(
		"gave up waiting after %s; %d Job(s) were left for the garbage collector: %s",
		elapsed.Round(time.Second), len(remaining), strings.Join(remaining, ", "))
	logger.Info("abandoning cleanup", "elapsed", elapsed.Round(time.Second), "jobs", remaining)

	// The status write comes first and the event second, because removing the
	// finalizer deletes the object and the condition with it. The event
	// outlives both, for as long as the cluster keeps events, and it is the
	// only durable record that this happened.
	status := pb.Status.DeepCopy()
	setCondition(status, pb.Generation, opsv1alpha1.ConditionDegraded, metav1.ConditionTrue,
		opsv1alpha1.ReasonCleanupAbandoned, message)
	if err := r.writeStatus(ctx, pb, status); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.event(pb, "Warning", opsv1alpha1.ReasonCleanupAbandoned, message)

	return ctrl.Result{}, r.releaseFinalizer(ctx, pb)
}

func (r *PostgresBackupReconciler) releaseFinalizer(ctx context.Context, pb *opsv1alpha1.PostgresBackup) error {
	controllerutil.RemoveFinalizer(pb, opsv1alpha1.Finalizer)
	if err := r.Update(ctx, pb); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// Gone, or someone else got there first. Either way the finalizer
			// is not this reconcile's problem any more.
			return nil
		}
		return fmt.Errorf("removing finalizer: %w", err)
	}
	return nil
}

// cleanupBackoff doubles from a second up to the ceiling as a function of how
// long cleanup has been going. It is derived from elapsed rather than held in
// a counter so that it survives a controller restart, and it is a plain
// function so the shape can be tested.
func cleanupBackoff(elapsed time.Duration) time.Duration {
	wait := time.Second
	for wait < elapsed/2 && wait < cleanupBackoffCeiling {
		wait *= 2
	}
	if wait > cleanupBackoffCeiling {
		return cleanupBackoffCeiling
	}
	return wait
}
