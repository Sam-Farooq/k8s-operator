// Package controller holds the PostgresBackup reconciler.
//
// Reconcile is called again whenever the API server says something changed,
// whenever the manager restarts, and on a timer this file chooses. Nothing
// tells it which of those happened, so it cannot act on what it did last
// time. Every decision below is a function of the spec, the clock, and the
// Jobs the API server currently reports.
package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
	"github.com/Sam-Farooq/k8s-operator/internal/schedule"
)

// requeueFloor and requeueCeiling bound the timer requeue.
//
// The ceiling matters: a schedule whose next slot is 23 hours away would
// otherwise produce one reconcile a day, and status.nextScheduleTime would
// sit unverified for that long. The floor stops a slot in the past from
// producing a tight loop.
const (
	requeueFloor   = 2 * time.Second
	requeueCeiling = 10 * time.Minute
)

// Clock is the only input to the reconciler that does not come from the API
// server. It is an interface so the tests can stand either side of a slot
// without waiting for one.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// PostgresBackupReconciler reconciles PostgresBackup objects.
type PostgresBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    Clock
}

// +kubebuilder:rbac:groups=ops.sam-farooq.dev,resources=postgresbackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=ops.sam-farooq.dev,resources=postgresbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ops.sam-farooq.dev,resources=postgresbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// There is deliberately no rule for secrets. The operator puts the Secret's
// name in the Job's environment and the kubelet reads it; no database
// password passes through this process, so no rule in this role allows one
// to.

// Reconcile brings one PostgresBackup towards its spec.
func (r *PostgresBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pb opsv1alpha1.PostgresBackup
	if err := r.Get(ctx, req.NamespacedName, &pb); err != nil {
		// NotFound here is the normal end of an object's life, not something
		// to retry.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	jobs, err := r.ownedJobs(ctx, &pb)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing owned Jobs: %w", err)
	}
	obs := observe(jobs)

	if !pb.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &pb, obs)
	}

	if !controllerutil.ContainsFinalizer(&pb, opsv1alpha1.Finalizer) {
		controllerutil.AddFinalizer(&pb, opsv1alpha1.Finalizer)
		if err := r.Update(ctx, &pb); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// That update changed resourceVersion. Carrying on with the copy in
		// hand would make the status write below fail on a conflict this
		// reconcile caused itself.
		return ctrl.Result{Requeue: true}, nil
	}

	sched, parseErr := schedule.Parse(pb.Spec.Schedule)
	if parseErr != nil {
		// A parse failure is terminal. Retrying it on a timer never produces
		// a different answer, and the watch on this object delivers the next
		// spec change without help, so this path returns no error and no
		// requeue on purpose.
		logger.Info("schedule will not parse", "schedule", pb.Spec.Schedule, "error", parseErr.Error())
		r.event(&pb, "Warning", opsv1alpha1.ReasonInvalidSchedule, parseErr.Error())
		status := r.statusFrom(&pb, obs, decision{act: actionNone}, nil)
		setCondition(status, pb.Generation, opsv1alpha1.ConditionReady, metav1.ConditionFalse,
			opsv1alpha1.ReasonInvalidSchedule, truncate(parseErr.Error(), 300))
		setCondition(status, pb.Generation, opsv1alpha1.ConditionDegraded, metav1.ConditionTrue,
			opsv1alpha1.ReasonInvalidSchedule, "spec.schedule does not parse, so no slot can be computed")
		return ctrl.Result{}, r.writeStatus(ctx, &pb, status)
	}

	now := r.Clock.Now().UTC()
	d := decide(&pb, sched, obs, now)

	switch d.act {
	case actionCreate:
		created, err := r.createJob(ctx, &pb, d.slot)
		switch {
		case err != nil:
			return ctrl.Result{}, err
		case created.collision:
			logger.Info("the Job name for this slot is taken by something else",
				"job", created.job.Name, "slot", d.slot.Format(time.RFC3339))
			r.event(&pb, "Warning", opsv1alpha1.ReasonJobNameCollision,
				fmt.Sprintf("Job %s exists and is not controlled by this PostgresBackup; the slot at %s was not run",
					created.job.Name, d.slot.Format(time.RFC3339)))
			d.collision = created.job.Name
			// Leaving lastScheduleTime where it is keeps the slot visibly
			// unhandled. Returning no error keeps a permanent condition out
			// of the rate limiter, where it would retry forever.
			d.advance = false
		default:
			logger.Info("created backup Job", "job", created.job.Name, "slot", d.slot.Format(time.RFC3339))
			r.event(&pb, "Normal", "JobCreated",
				fmt.Sprintf("Created Job %s for the slot at %s", created.job.Name, d.slot.Format(time.RFC3339)))
			// Fold the new Job into the observation so the status written
			// below already names it. Re-listing would be the obvious way and
			// the wrong one: in the manager the list comes from a cache that
			// has not seen the create yet.
			jobs = append(jobs, *created.job)
			obs = observe(jobs)
		}
	case actionSkip:
		logger.Info("slot skipped", "slot", d.slot.Format(time.RFC3339), "reason", d.reason)
		r.event(&pb, "Warning", d.reason, d.message)
	case actionAdopt, actionNone:
	}

	if pruned, err := r.prune(ctx, &pb, obs); err != nil {
		return ctrl.Result{}, err
	} else if pruned > 0 {
		// The deletes are in flight. The Jobs still exist with a deletion
		// timestamp, so they stay in this pass's status and leave it on the
		// watch event for their removal.
		logger.Info("retention asked for finished Jobs to be deleted", "count", pruned)
	}

	var next *time.Time
	if t, err := sched.Next(now); err == nil {
		next = &t
	}

	status := r.statusFrom(&pb, obs, d, next)
	setConditions(status, &pb, obs, d)
	if err := r.writeStatus(ctx, &pb, status); err != nil {
		return ctrl.Result{}, err
	}

	if next == nil {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: requeueIn(now, *next)}, nil
}

// statusFrom builds the whole status out of the observation, the decision and
// the next slot. It is a function rather than a sequence of assignments
// because every field has to be derivable from those three things; a field
// that needs the previous status is a field that goes wrong after a restart.
//
// The two exceptions are explicit. lastScheduleTime only moves forward, and
// skippedSlots only counts up, so both read the old status.
func (r *PostgresBackupReconciler) statusFrom(pb *opsv1alpha1.PostgresBackup, obs observation, d decision, next *time.Time) *opsv1alpha1.PostgresBackupStatus {
	status := pb.Status.DeepCopy()
	status.ObservedGeneration = pb.Generation
	status.History = obs.history()
	status.ActiveJobs = obs.activeNames()
	status.LastSuccessfulTime = obs.lastSuccessful()
	status.NextScheduleTime = nil
	if next != nil {
		t := metav1.NewTime(*next)
		status.NextScheduleTime = &t
	}
	if d.advance {
		t := metav1.NewTime(d.slot)
		status.LastScheduleTime = &t
	}
	status.SkippedSlots += d.skipped
	return status
}

// requeueIn is the timer for the next reconcile: a second past the next slot,
// clamped. It is a plain function so the bounds can be tested without a
// manager.
func requeueIn(now, next time.Time) time.Duration {
	d := next.Add(time.Second).Sub(now)
	if d < requeueFloor {
		return requeueFloor
	}
	if d > requeueCeiling {
		return requeueCeiling
	}
	return d
}

// ownedJobs returns the Jobs in the namespace whose controller reference is
// this object.
//
// The list is not narrowed by a label selector, which costs a full namespace
// list. A selector would be cheaper and would make the answer depend on a
// label: anyone with patch on Jobs could remove it, the Job would become
// invisible to the reconciler, and it would never be counted for concurrency,
// never pruned by retention and never cleaned up on delete. The owner
// reference is the thing the garbage collector acts on, so it is the thing
// membership is decided by. Under a manager the list is served from the cache
// that `Owns` already fills, so the cost is a map walk rather than a request.
//
// The match is on UID, not name. A PostgresBackup deleted and recreated under
// the same name is a different object and does not inherit the old one's
// Jobs.
func (r *PostgresBackupReconciler) ownedJobs(ctx context.Context, pb *opsv1alpha1.PostgresBackup) ([]batchv1.Job, error) {
	var list batchv1.JobList
	if err := r.List(ctx, &list, client.InNamespace(pb.Namespace)); err != nil {
		return nil, err
	}
	owned := make([]batchv1.Job, 0, len(list.Items))
	for i := range list.Items {
		owner := metav1.GetControllerOf(&list.Items[i])
		if owner == nil || owner.UID != pb.UID {
			continue
		}
		owned = append(owned, list.Items[i])
	}
	return owned, nil
}

type createResult struct {
	job       *batchv1.Job
	collision bool
}

// createJob creates the Job for a slot. The name is a pure function of the
// owner and the slot, so a second call for the same slot comes back
// AlreadyExists rather than starting a second dump.
func (r *PostgresBackupReconciler) createJob(ctx context.Context, pb *opsv1alpha1.PostgresBackup, slot time.Time) (createResult, error) {
	job := BuildJob(pb, slot)
	res := createResult{job: job}
	// The owner reference is what makes the garbage collector delete this Job
	// with its PostgresBackup, and what ownedJobs reads membership from.
	if err := controllerutil.SetControllerReference(pb, job, r.Scheme); err != nil {
		return res, fmt.Errorf("setting controller reference on %s: %w", job.Name, err)
	}

	err := r.Create(ctx, job)
	if err == nil {
		return res, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return res, fmt.Errorf("creating Job %s: %w", job.Name, err)
	}

	// AlreadyExists is either this reconcile racing another copy of itself,
	// which is the mechanism working, or a name collision with something that
	// is not ours, which is not. The owner reference tells them apart.
	var existing batchv1.Job
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			// Deleted between the Create and the Get. The next reconcile
			// creates it again.
			return res, nil
		}
		return res, fmt.Errorf("reading the Job that already exists, %s: %w", job.Name, err)
	}
	if owner := metav1.GetControllerOf(&existing); owner != nil && owner.UID == pb.UID {
		res.job = &existing
		return res, nil
	}
	res.collision = true
	return res, nil
}

// prune deletes finished Jobs outside the retention window and returns how
// many deletes were accepted.
func (r *PostgresBackupReconciler) prune(ctx context.Context, pb *opsv1alpha1.PostgresBackup, obs observation) (int, error) {
	deleted := 0
	for _, job := range obs.prunable(pb.Spec.Retention.KeepSucceededOrDefault(), pb.Spec.Retention.KeepFailedOrDefault()) {
		if !job.DeletionTimestamp.IsZero() {
			continue
		}
		// The UID precondition stops a delete racing a recreate: without it a
		// Job recreated under the same name between the list and the delete
		// is the one that goes.
		err := r.Delete(ctx, job,
			client.PropagationPolicy(metav1.DeletePropagationBackground),
			client.Preconditions{UID: &job.UID},
		)
		switch {
		case err == nil:
			deleted++
		case apierrors.IsNotFound(err), apierrors.IsConflict(err):
			// Someone got there first, or the UID moved. Either way the Job
			// this reconcile meant to delete is not there any more.
		default:
			return deleted, fmt.Errorf("deleting Job %s: %w", job.Name, err)
		}
	}
	return deleted, nil
}

// writeStatus writes the status subresource, and only when something in it
// moved.
//
// The guard is not an optimisation. A status write is a change to the object,
// the controller watches the object, so an unconditional write is a reconcile
// loop that never stops and a bill that never stops either. It is also what
// makes "the second call changed nothing" a thing a test can assert.
func (r *PostgresBackupReconciler) writeStatus(ctx context.Context, pb *opsv1alpha1.PostgresBackup, want *opsv1alpha1.PostgresBackupStatus) error {
	if equality.Semantic.DeepEqual(&pb.Status, want) {
		return nil
	}
	pb.Status = *want
	if err := r.Status().Update(ctx, pb); err != nil {
		// A conflict means someone wrote first. Returning it requeues through
		// the controller's own rate limiter, which is the backoff.
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

func (r *PostgresBackupReconciler) event(pb *opsv1alpha1.PostgresBackup, kind, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(pb, kind, reason, truncate(message, 1024))
}

// SetupWithManager wires the watches.
func (r *PostgresBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = realClock{}
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("postgresbackup-controller")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&opsv1alpha1.PostgresBackup{}).
		// Owns is what makes a finished dump land in status without polling
		// for it: the Job's status moving is a reconcile of its owner.
		Owns(&batchv1.Job{}).
		Named("postgresbackup").
		Complete(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
