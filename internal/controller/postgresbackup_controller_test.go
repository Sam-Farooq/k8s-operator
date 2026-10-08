package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
	"github.com/Sam-Farooq/k8s-operator/internal/schedule"
)

// harness is one PostgresBackup in its own namespace, a reconciler pointed at
// it, and a clock the test moves by hand.
type harness struct {
	t      *testing.T
	c      client.Client
	r      *PostgresBackupReconciler
	clock  *fakeClock
	events *record.FakeRecorder
	key    types.NamespacedName
	slot   time.Time // the first slot strictly after the object was created
}

// newHarness creates a PostgresBackup on an every-minute schedule. Every
// minute is used rather than a nightly one because the object's creation
// timestamp is the floor for the first slot, and that timestamp comes from
// the API server's clock, not from the fake one.
func newHarness(t *testing.T, mutate func(*opsv1alpha1.PostgresBackup)) *harness {
	t.Helper()
	c := env(t)
	ns := namespace(t, c)

	pb := sample()
	pb.Namespace = ns
	pb.Spec.Schedule = "* * * * *"
	pb.Spec.Retention.KeepSucceeded = 7
	keepFailed := int32(3)
	pb.Spec.Retention.KeepFailed = &keepFailed
	if mutate != nil {
		mutate(pb)
	}
	if err := c.Create(context.Background(), pb); err != nil {
		t.Fatalf("creating the PostgresBackup: %v", err)
	}

	// The floor for the first slot is the creation timestamp, which comes
	// from the API server's clock rather than the fake one, so the first slot
	// has to be computed from it.
	sched, err := schedule.Parse(pb.Spec.Schedule)
	if err != nil {
		// An invalid schedule is a case under test; the harness then has no
		// slot and the clock is simply now.
		broken := &harness{
			t:      t,
			c:      c,
			clock:  &fakeClock{now: time.Now().UTC()},
			events: record.NewFakeRecorder(64),
			key:    client.ObjectKeyFromObject(pb),
		}
		broken.r = &PostgresBackupReconciler{
			Client: c, Scheme: scheme, Clock: broken.clock, Recorder: broken.events,
		}
		return broken
	}
	first, err := sched.Next(pb.CreationTimestamp.UTC())
	if err != nil {
		t.Fatalf("the fixture schedule has no next slot: %v", err)
	}

	h := &harness{
		t:      t,
		c:      c,
		clock:  &fakeClock{now: first.Add(10 * time.Second)},
		events: record.NewFakeRecorder(64),
		key:    client.ObjectKeyFromObject(pb),
		slot:   first,
	}
	h.r = &PostgresBackupReconciler{
		Client:   c,
		Scheme:   scheme,
		Clock:    h.clock,
		Recorder: h.events,
	}
	return h
}

func (h *harness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		h.t.Fatalf("Reconcile returned %v", err)
	}
	return res
}

// settle runs Reconcile until it stops changing the object, which is the
// first reconcile after the finalizer is added. It returns the number of
// calls it took.
func (h *harness) settle() int {
	h.t.Helper()
	for i := 1; i <= 5; i++ {
		before := h.get().ResourceVersion
		h.reconcile()
		if h.get().ResourceVersion == before {
			return i
		}
	}
	h.t.Fatal("Reconcile never stopped writing to the object")
	return 0
}

// sawEvent drains the recorder and reports whether any event mentions the
// reason. FakeRecorder formats each one as "Type Reason Message".
func (h *harness) sawEvent(reason string) bool {
	h.t.Helper()
	found := false
	for {
		select {
		case e := <-h.events.Events:
			if strings.Contains(e, reason) {
				found = true
			}
		default:
			return found
		}
	}
}

func (h *harness) get() *opsv1alpha1.PostgresBackup {
	h.t.Helper()
	var pb opsv1alpha1.PostgresBackup
	if err := h.c.Get(context.Background(), h.key, &pb); err != nil {
		h.t.Fatalf("reading the PostgresBackup: %v", err)
	}
	return &pb
}

func (h *harness) jobs() []batchv1.Job {
	h.t.Helper()
	var list batchv1.JobList
	if err := h.c.List(context.Background(), &list, client.InNamespace(h.key.Namespace)); err != nil {
		h.t.Fatalf("listing Jobs: %v", err)
	}
	return list.Items
}

// finish writes a terminal condition onto a Job the way the Job controller
// would. envtest runs an API server and no controllers, so nothing else will.
func (h *harness) finish(job *batchv1.Job, succeeded bool) {
	h.t.Helper()
	start := metav1.NewTime(h.clock.Now())
	job.Status.StartTime = &start
	// Since 1.31 the API server refuses Complete=True without
	// SuccessCriteriaMet=True, and Failed=True without FailureTarget=True.
	// Those two are "this Job is going to end that way" and the controller
	// deliberately does not read them: a Job with FailureTarget set is still
	// running, and reporting it as failed would mark a backup failed while
	// its last retry was in flight.
	if succeeded {
		done := metav1.NewTime(h.clock.Now().Add(30 * time.Second))
		job.Status.CompletionTime = &done
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:               batchv1.JobSuccessCriteriaMet,
				Status:             corev1.ConditionTrue,
				Reason:             "CompletionsReached",
				LastTransitionTime: done,
			},
			{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				Reason:             "CompletionsReached",
				LastTransitionTime: done,
			},
		}
	} else {
		at := metav1.NewTime(h.clock.Now())
		job.Status.Failed = 3
		job.Status.Conditions = []batchv1.JobCondition{
			{
				Type:               batchv1.JobFailureTarget,
				Status:             corev1.ConditionTrue,
				Reason:             "BackoffLimitExceeded",
				LastTransitionTime: at,
			},
			{
				Type:               batchv1.JobFailed,
				Status:             corev1.ConditionTrue,
				Reason:             "BackoffLimitExceeded",
				LastTransitionTime: at,
			},
		}
	}
	if err := h.c.Status().Update(context.Background(), job); err != nil {
		h.t.Fatalf("writing Job status: %v", err)
	}
}

func TestReconcileCreatesTheJobForTheDueSlot(t *testing.T) {
	h := newHarness(t, nil)

	// The first call only adds the finalizer, because the update it makes
	// invalidates the copy it is holding.
	first := h.reconcile()
	if !first.Requeue {
		t.Fatal("the finalizer pass did not ask to be requeued")
	}
	if n := len(h.jobs()); n != 0 {
		t.Fatalf("the finalizer pass created %d Jobs", n)
	}
	if !containsFinalizer(h.get()) {
		t.Fatal("the finalizer was not added")
	}

	h.reconcile()
	jobs := h.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Jobs after the second pass, want 1", len(jobs))
	}
	job := jobs[0]
	if want := JobName(h.key.Name, h.slot); job.Name != want {
		t.Errorf("Job name = %q, want %q", job.Name, want)
	}

	owner := metav1.GetControllerOf(&job)
	if owner == nil {
		t.Fatal("the Job has no controller owner reference, so nothing garbage collects it")
	}
	if owner.UID != h.get().UID {
		t.Errorf("owner UID = %s, want the PostgresBackup's", owner.UID)
	}
	if owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		t.Error("blockOwnerDeletion is not set on the owner reference")
	}

	pb := h.get()
	if pb.Status.LastScheduleTime == nil || !pb.Status.LastScheduleTime.Time.UTC().Equal(h.slot) {
		t.Errorf("lastScheduleTime = %v, want %s", pb.Status.LastScheduleTime, h.slot)
	}
	if len(pb.Status.ActiveJobs) != 1 || pb.Status.ActiveJobs[0] != job.Name {
		t.Errorf("activeJobs = %v, want the Job it just created", pb.Status.ActiveJobs)
	}
	if len(pb.Status.History) != 1 || pb.Status.History[0].Phase != opsv1alpha1.RunRunning {
		t.Errorf("history = %v", pb.Status.History)
	}
	if pb.Status.ObservedGeneration != pb.Generation {
		t.Errorf("observedGeneration = %d, generation = %d", pb.Status.ObservedGeneration, pb.Generation)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	calls := h.settle()
	t.Logf("settled after %d Reconcile calls", calls)

	jobs := h.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Jobs after settling, want 1", len(jobs))
	}
	beforeObject := h.get().ResourceVersion
	beforeJob := jobs[0].ResourceVersion
	beforeUID := jobs[0].UID

	// The clock does not move. Reconcile is called again with exactly the
	// state it left behind, which is what the controller-runtime workqueue
	// does on any unrelated watch event.
	res := h.reconcile()

	after := h.jobs()
	if len(after) != 1 {
		t.Fatalf("the second call left %d Jobs, want 1", len(after))
	}
	if after[0].UID != beforeUID {
		t.Error("the second call replaced the Job rather than leaving it alone")
	}
	if after[0].ResourceVersion != beforeJob {
		t.Errorf("the second call wrote to the Job: resourceVersion %s then %s", beforeJob, after[0].ResourceVersion)
	}
	if got := h.get().ResourceVersion; got != beforeObject {
		t.Errorf("the second call wrote to the PostgresBackup: resourceVersion %s then %s", beforeObject, got)
	}
	if res.RequeueAfter < requeueFloor || res.RequeueAfter > requeueCeiling {
		t.Errorf("requeueAfter = %s, outside the bounds", res.RequeueAfter)
	}

	// And a third and fourth, because an alternating pair of states would
	// pass the check above.
	for i := 3; i <= 4; i++ {
		h.reconcile()
		if got := h.get().ResourceVersion; got != beforeObject {
			t.Fatalf("call %d wrote to the PostgresBackup", i)
		}
		if n := len(h.jobs()); n != 1 {
			t.Fatalf("call %d left %d Jobs", i, n)
		}
	}
}

func TestReconcileDoesNotRecreateAJobForAHandledSlot(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()

	jobs := h.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Jobs, want 1", len(jobs))
	}
	// Background propagation on purpose. A Job deleted with the default
	// policy gets an orphan finalizer that only the garbage collector
	// removes, and envtest runs an API server with no controllers, so it
	// would sit there Terminating and the test would be asserting the wrong
	// thing.
	if err := h.c.Delete(context.Background(), &jobs[0],
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatalf("deleting the Job by hand: %v", err)
	}

	// The slot is recorded in lastScheduleTime, so it is over. A controller
	// that recreated it would run the dump again every time somebody tidied
	// up a Job, and would never stop.
	h.reconcile()
	if n := len(h.jobs()); n != 0 {
		t.Fatalf("%d Jobs after the slot's Job was deleted, want 0", n)
	}
}

func TestReconcileRunsTheNextSlotWhenTheClockMoves(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()

	// The first dump has to finish first: the fixture's concurrencyPolicy is
	// Forbid, which is what the next test is about.
	jobs := h.jobs()
	h.finish(&jobs[0], true)

	h.clock.advance(time.Minute)
	h.reconcile()

	jobs = h.jobs()
	if len(jobs) != 2 {
		t.Fatalf("%d Jobs after the clock moved a minute, want 2", len(jobs))
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.Labels[opsv1alpha1.LabelSlot]] = true
	}
	if len(seen) != 2 {
		t.Errorf("the two Jobs carry %d distinct slot labels", len(seen))
	}
}

func TestReconcileRecordsASuccessfulRun(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()

	jobs := h.jobs()
	h.finish(&jobs[0], true)
	h.reconcile()

	pb := h.get()
	if len(pb.Status.History) != 1 {
		t.Fatalf("history = %v", pb.Status.History)
	}
	rec := pb.Status.History[0]
	if rec.Phase != opsv1alpha1.RunSucceeded {
		t.Errorf("phase = %s, want Succeeded", rec.Phase)
	}
	if rec.CompletionTime == nil {
		t.Error("the record has no completion time")
	}
	if pb.Status.LastSuccessfulTime == nil {
		t.Fatal("lastSuccessfulTime is still nil after a success")
	}
	if len(pb.Status.ActiveJobs) != 0 {
		t.Errorf("activeJobs = %v, want empty", pb.Status.ActiveJobs)
	}

	ready := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %v, want True", ready)
	}
	running := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionRunning)
	if running == nil || running.Status != metav1.ConditionFalse {
		t.Fatalf("Running = %v, want False", running)
	}
}

func TestReconcileRecordsAFailedRun(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()

	jobs := h.jobs()
	h.finish(&jobs[0], false)
	h.reconcile()

	pb := h.get()
	if pb.Status.LastSuccessfulTime != nil {
		t.Error("lastSuccessfulTime was set by a failure")
	}
	degraded := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded = %v, want True", degraded)
	}
	if degraded.Reason != opsv1alpha1.ReasonLastRunFailed {
		t.Errorf("Degraded reason = %q", degraded.Reason)
	}
}

func TestReconcileStaleStatusIsDetectable(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()

	pb := h.get()
	generationBefore := pb.Generation
	pb.Spec.Retention.KeepFailed = ptr(int32(5))
	if err := h.c.Update(context.Background(), pb); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}

	updated := h.get()
	if updated.Generation == generationBefore {
		t.Fatal("the spec change did not bump metadata.generation")
	}
	// Before the controller has looked, every condition still reports the old
	// generation. That is the whole point of the field.
	for _, c := range updated.Status.Conditions {
		if c.ObservedGeneration >= updated.Generation {
			t.Errorf("condition %s already claims generation %d", c.Type, c.ObservedGeneration)
		}
	}

	h.reconcile()
	after := h.get()
	if after.Status.ObservedGeneration != after.Generation {
		t.Errorf("status.observedGeneration = %d, generation = %d", after.Status.ObservedGeneration, after.Generation)
	}
	for _, c := range after.Status.Conditions {
		if c.ObservedGeneration != after.Generation {
			t.Errorf("condition %s carries generation %d, want %d", c.Type, c.ObservedGeneration, after.Generation)
		}
	}
}

func TestReconcileSuspendCreatesNothing(t *testing.T) {
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) { pb.Spec.Suspend = true })
	h.settle()

	if n := len(h.jobs()); n != 0 {
		t.Fatalf("%d Jobs while suspended, want 0", n)
	}
	pb := h.get()
	if pb.Status.SkippedSlots == 0 {
		t.Error("the suspended slot was not counted in skippedSlots")
	}
	ready := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != opsv1alpha1.ReasonSuspended {
		t.Fatalf("Ready = %v, want False/Suspended", ready)
	}
	degraded := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded = %v, want False: suspension is intentional", degraded)
	}
}

func TestReconcileStartingDeadlineSkipsAStaleSlot(t *testing.T) {
	// An hourly schedule, not the every-minute fixture. With a slot every
	// minute the most recent one is never more than a minute old, so no
	// starting deadline can ever be exceeded and the test would be asserting
	// nothing.
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) {
		pb.Spec.Schedule = "0 * * * *"
		pb.Spec.StartingDeadlineSeconds = 60
	})
	h.clock.now = h.slot.Add(10 * time.Minute)
	h.settle()

	if n := len(h.jobs()); n != 0 {
		t.Fatalf("%d Jobs for a slot ten minutes past a one minute deadline, want 0", n)
	}
	pb := h.get()
	if pb.Status.SkippedSlots < 1 {
		t.Errorf("skippedSlots = %d, want at least 1", pb.Status.SkippedSlots)
	}
	if !h.sawEvent(opsv1alpha1.ReasonDeadlineExceeded) {
		t.Error("no event said the slot was past its starting deadline")
	}
	// The skip is durable in skippedSlots and in the event, not in a
	// condition that the next reconcile would clear.
	degraded := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded = %v, want False", degraded)
	}
}

func TestReconcileConcurrencyForbidSkipsTheSlot(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()
	if n := len(h.jobs()); n != 1 {
		t.Fatalf("%d Jobs, want 1", n)
	}

	// The first Job is left running, and the next slot arrives.
	h.clock.advance(time.Minute)
	h.reconcile()

	if n := len(h.jobs()); n != 1 {
		t.Fatalf("%d Jobs with a dump still running and Forbid set, want 1", n)
	}
	if !h.sawEvent(opsv1alpha1.ReasonConcurrencyForbidden) {
		t.Error("no event said the slot was blocked by the running Job")
	}
	if got := h.get().Status.SkippedSlots; got != 1 {
		t.Errorf("skippedSlots = %d, want 1", got)
	}
}

func TestReconcileConcurrencyAllowStartsTheSlot(t *testing.T) {
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) {
		pb.Spec.ConcurrencyPolicy = opsv1alpha1.ConcurrencyAllow
	})
	h.settle()
	h.clock.advance(time.Minute)
	h.reconcile()

	if n := len(h.jobs()); n != 2 {
		t.Fatalf("%d Jobs with Allow set, want 2", n)
	}
}

func TestReconcileRetentionPrunesTheOldest(t *testing.T) {
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) {
		pb.Spec.Retention.KeepSucceeded = 2
		none := int32(0)
		pb.Spec.Retention.KeepFailed = &none
	})
	h.settle()

	// Four minutes, four Jobs, each finished before the next slot.
	for i := 0; i < 3; i++ {
		jobs := h.jobs()
		h.finish(&jobs[len(jobs)-1], i != 1) // the second one fails
		h.clock.advance(time.Minute)
		h.reconcile()
	}
	jobs := h.jobs()
	h.finish(&jobs[len(jobs)-1], true)
	h.reconcile()

	remaining := h.jobs()
	if len(remaining) != 2 {
		names := []string{}
		for _, j := range remaining {
			names = append(names, j.Name)
		}
		t.Fatalf("%d Jobs after retention with keepSucceeded 2 and keepFailed 0: %v", len(remaining), names)
	}
	for _, j := range remaining {
		if phase, _ := phaseOf(&j); phase != opsv1alpha1.RunSucceeded {
			t.Errorf("Job %s survived with phase %s, and keepFailed is 0", j.Name, phase)
		}
	}
}

func TestReconcileInvalidScheduleIsTerminal(t *testing.T) {
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) { pb.Spec.Schedule = "every tuesday at two" })
	h.reconcile() // finalizer
	res := h.reconcile()

	if res.RequeueAfter != 0 || res.Requeue {
		t.Errorf("result = %+v, want no requeue: retrying a parse failure on a timer never changes the answer", res)
	}
	if n := len(h.jobs()); n != 0 {
		t.Fatalf("%d Jobs on an unparseable schedule", n)
	}
	pb := h.get()
	if pb.Status.NextScheduleTime != nil {
		t.Error("nextScheduleTime is set for a schedule that does not parse")
	}
	ready := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionReady)
	if ready == nil || ready.Reason != opsv1alpha1.ReasonInvalidSchedule {
		t.Fatalf("Ready = %v, want False/InvalidSchedule", ready)
	}
}

func TestReconcileJobNameCollisionIsReportedNotAdopted(t *testing.T) {
	h := newHarness(t, nil)
	h.reconcile() // finalizer only

	// Something else already holds the name this slot would use.
	squatter := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      JobName(h.key.Name, h.slot),
			Namespace: h.key.Namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "x", Image: "busybox"}},
				},
			},
		},
	}
	if err := h.c.Create(context.Background(), squatter); err != nil {
		t.Fatalf("creating the squatter Job: %v", err)
	}

	h.reconcile()

	pb := h.get()
	degraded := meta.FindStatusCondition(pb.Status.Conditions, opsv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Reason != opsv1alpha1.ReasonJobNameCollision {
		t.Fatalf("Degraded = %v, want JobNameCollision", degraded)
	}
	if pb.Status.LastScheduleTime != nil {
		t.Error("lastScheduleTime advanced over a slot that was never run")
	}
	if len(pb.Status.History) != 0 {
		t.Errorf("history = %v, want empty: the squatter is not ours", pb.Status.History)
	}
	if !h.sawEvent(opsv1alpha1.ReasonJobNameCollision) {
		t.Error("no event named the Job that was in the way")
	}

	// Unlike a skipped slot, this one has to stick: lastScheduleTime was not
	// advanced, so the next reconcile reaches the same conclusion.
	h.reconcile()
	again := meta.FindStatusCondition(h.get().Status.Conditions, opsv1alpha1.ConditionDegraded)
	if again == nil || again.Reason != opsv1alpha1.ReasonJobNameCollision {
		t.Fatalf("Degraded after a second pass = %v, want it to still report the collision", again)
	}

	// And the squatter is untouched.
	var after batchv1.Job
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(squatter), &after); err != nil {
		t.Fatalf("the squatter Job is gone: %v", err)
	}
	if metav1.GetControllerOf(&after) != nil {
		t.Error("the operator adopted a Job it did not create")
	}
}

func TestFinalizerDeletesTheJobsThenReleases(t *testing.T) {
	h := newHarness(t, nil)
	h.settle()
	if n := len(h.jobs()); n != 1 {
		t.Fatalf("%d Jobs, want 1", n)
	}

	if err := h.c.Delete(context.Background(), h.get()); err != nil {
		t.Fatalf("deleting the PostgresBackup: %v", err)
	}
	pb := h.get()
	if pb.DeletionTimestamp.IsZero() {
		t.Fatal("the finalizer did not hold the object")
	}

	res := h.reconcile()
	if res.RequeueAfter == 0 {
		t.Error("cleanup did not ask to come back, so a stuck Job would never be noticed")
	}
	if n := len(h.jobs()); n != 0 {
		t.Fatalf("%d Jobs after the cleanup pass", n)
	}

	h.reconcile()
	var gone opsv1alpha1.PostgresBackup
	err := h.c.Get(context.Background(), h.key, &gone)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the PostgresBackup is still there: %v", err)
	}
}

func TestFinalizerGivesUpOnAJobThatWillNotDelete(t *testing.T) {
	// cleanupTimeoutSeconds 0 is the degenerate case of the escape hatch, and
	// the one a test can assert without waiting.
	h := newHarness(t, func(pb *opsv1alpha1.PostgresBackup) {
		never := int64(0)
		pb.Spec.CleanupTimeoutSeconds = &never
	})
	h.settle()

	jobs := h.jobs()
	if len(jobs) != 1 {
		t.Fatalf("%d Jobs, want 1", len(jobs))
	}
	// A finalizer of somebody else's on the Job is how a pod stuck on an
	// unresponsive volume looks from here: the delete is accepted and nothing
	// goes away.
	stuck := jobs[0]
	stuck.Finalizers = append(stuck.Finalizers, "test.example/hold")
	if err := h.c.Update(context.Background(), &stuck); err != nil {
		t.Fatalf("holding the Job: %v", err)
	}

	if err := h.c.Delete(context.Background(), h.get()); err != nil {
		t.Fatalf("deleting the PostgresBackup: %v", err)
	}
	h.reconcile()

	var gone opsv1alpha1.PostgresBackup
	if err := h.c.Get(context.Background(), h.key, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("the PostgresBackup survived a Job that will not delete: %v", err)
	}

	var held batchv1.Job
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: stuck.Namespace, Name: stuck.Name}, &held); err != nil {
		t.Fatalf("reading the held Job: %v", err)
	}
	if held.DeletionTimestamp.IsZero() {
		t.Error("the operator removed its finalizer without ever asking for the Job to go")
	}

	held.Finalizers = nil
	if err := h.c.Update(context.Background(), &held); err != nil {
		t.Fatalf("releasing the held Job: %v", err)
	}
}

func TestReconcileOnAMissingObjectIsNotAnError(t *testing.T) {
	c := env(t)
	r := &PostgresBackupReconciler{Client: c, Scheme: scheme, Clock: &fakeClock{now: time.Now().UTC()}}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "never-existed"},
	})
	if err != nil {
		t.Fatalf("Reconcile on a deleted object returned %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("result = %+v, want nothing to do", res)
	}
}

func TestCRDRejectsWhatTheTypesSayItShould(t *testing.T) {
	// The CRD is generated, so these are assertions about the generator's
	// output and about the markers on the Go types, which nothing else checks.
	c := env(t)
	ns := namespace(t, c)

	cases := map[string]func(*opsv1alpha1.PostgresBackup){
		"a Replace concurrency policy": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.ConcurrencyPolicy = "Replace"
		},
		"an sslMode that libpq does not have": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.Source.SSLMode = "maybe"
		},
		"a prefix with a slash in it": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.Destination.Prefix = "orders/daily"
		},
		"keepSucceeded above the maximum": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.Retention.KeepSucceeded = 101
		},
		"keepFailed above the maximum": func(pb *opsv1alpha1.PostgresBackup) {
			tooMany := int32(101)
			pb.Spec.Retention.KeepFailed = &tooMany
		},
		"a starting deadline under a minute": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.StartingDeadlineSeconds = 30
		},
		"an empty image": func(pb *opsv1alpha1.PostgresBackup) {
			pb.Spec.Image = ""
		},
	}
	for name, mutate := range cases {
		pb := sample()
		pb.Namespace = ns
		pb.Name = "reject-" + sanitise(name)
		mutate(pb)
		if err := c.Create(context.Background(), pb); err == nil {
			t.Errorf("the API server accepted %s", name)
			_ = c.Delete(context.Background(), pb)
		}
	}
}

func TestCRDDefaultsTheFieldsWithDefaults(t *testing.T) {
	c := env(t)
	ns := namespace(t, c)

	pb := &opsv1alpha1.PostgresBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns},
		Spec: opsv1alpha1.PostgresBackupSpec{
			Schedule:    "30 2 * * *",
			Image:       "postgres:17.2-alpine",
			Source:      opsv1alpha1.SourceSpec{Host: "db", Database: "orders", SecretName: "creds"},
			Destination: opsv1alpha1.DestinationSpec{ClaimName: "pg-backups", Prefix: "orders"},
		},
	}
	if err := c.Create(context.Background(), pb); err != nil {
		t.Fatalf("creating a minimal PostgresBackup: %v", err)
	}

	if pb.Spec.Source.Port != 5432 || pb.Spec.Source.SSLMode != "require" {
		t.Errorf("source defaults = %+v", pb.Spec.Source)
	}
	if pb.Spec.ConcurrencyPolicy != opsv1alpha1.ConcurrencyForbid {
		t.Errorf("concurrencyPolicy = %q, want Forbid", pb.Spec.ConcurrencyPolicy)
	}
	if pb.Spec.Retention.KeepSucceeded != 7 || pb.Spec.Retention.KeepFailedOrDefault() != 3 {
		t.Errorf("retention defaults = keepSucceeded %d, keepFailed %v", pb.Spec.Retention.KeepSucceeded, pb.Spec.Retention.KeepFailed)
	}
	if pb.Spec.Retention.KeepFailed == nil {
		t.Error("keepFailed came back as nil, so the API server applied no default to the pointer")
	}
	// The pointer is the point: pruneFiles has a true default, so a plain
	// bool would make `pruneFiles: false` unexpressible.
	if pb.Spec.Retention.PruneFiles == nil || !*pb.Spec.Retention.PruneFiles {
		t.Errorf("pruneFiles = %v, want a pointer to true", pb.Spec.Retention.PruneFiles)
	}

	off := false
	pb.Spec.Retention.PruneFiles = &off
	if err := c.Update(context.Background(), pb); err != nil {
		t.Fatalf("setting pruneFiles to false: %v", err)
	}
	var back opsv1alpha1.PostgresBackup
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pb), &back); err != nil {
		t.Fatal(err)
	}
	if back.Spec.Retention.PruneFiles == nil || *back.Spec.Retention.PruneFiles {
		t.Errorf("pruneFiles came back as %v after being set to false", back.Spec.Retention.PruneFiles)
	}
}

func containsFinalizer(pb *opsv1alpha1.PostgresBackup) bool {
	for _, f := range pb.Finalizers {
		if f == opsv1alpha1.Finalizer {
			return true
		}
	}
	return false
}

func sanitise(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}
