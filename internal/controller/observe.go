package controller

import (
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// run is one owned Job, read.
type run struct {
	job    *batchv1.Job
	slot   time.Time
	phase  opsv1alpha1.RunPhase
	reason string
}

// observation is everything the reconciler knows about the world, derived
// entirely from the Jobs the API server returned. Nothing in here comes from
// status, which is what lets a reconcile be correct on its first call after a
// restart, after a status wipe, or after somebody deleted a Job by hand.
type observation struct {
	runs      []run
	active    []run
	succeeded []run
	failed    []run
	bySlot    map[string]*run
}

// observe classifies Jobs and sorts them newest slot first.
//
// The slot comes off the label rather than out of the Job name, because a
// name is a formatting decision and a label is data. A Job whose label is
// missing or unparseable falls back to its creation timestamp: it is still
// owned, so it still counts towards concurrency and retention, and pretending
// not to see it would leak it.
func observe(jobs []batchv1.Job) observation {
	obs := observation{bySlot: make(map[string]*run, len(jobs))}
	for i := range jobs {
		job := &jobs[i]
		r := run{job: job, slot: slotOf(job)}
		r.phase, r.reason = phaseOf(job)
		obs.runs = append(obs.runs, r)
	}
	sort.SliceStable(obs.runs, func(i, j int) bool {
		if obs.runs[i].slot.Equal(obs.runs[j].slot) {
			return obs.runs[i].job.Name < obs.runs[j].job.Name
		}
		return obs.runs[i].slot.After(obs.runs[j].slot)
	})
	for i := range obs.runs {
		r := &obs.runs[i]
		if label, ok := r.job.Labels[opsv1alpha1.LabelSlot]; ok {
			// First wins, and the list is newest first, so a duplicate label
			// resolves to the Job with the lexically smaller name rather than
			// to whichever order the API server happened to return.
			if _, seen := obs.bySlot[label]; !seen {
				obs.bySlot[label] = r
			}
		}
		switch r.phase {
		case opsv1alpha1.RunSucceeded:
			obs.succeeded = append(obs.succeeded, *r)
		case opsv1alpha1.RunFailed:
			obs.failed = append(obs.failed, *r)
		default:
			obs.active = append(obs.active, *r)
		}
	}
	return obs
}

// slotOf reads the slot label, falling back to the creation timestamp.
func slotOf(job *batchv1.Job) time.Time {
	if label, ok := job.Labels[opsv1alpha1.LabelSlot]; ok {
		if t, err := time.Parse(slotLayout, label); err == nil {
			return t.UTC()
		}
	}
	return job.CreationTimestamp.UTC()
}

// phaseOf reads the Job's own conditions. Counting job.Status.Succeeded or
// job.Status.Failed instead is the mistake worth avoiding: those are pod
// counters, and a Job with backoffLimit 2 shows Failed: 1 while it is still
// retrying and still going to succeed.
func phaseOf(job *batchv1.Job) (opsv1alpha1.RunPhase, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return opsv1alpha1.RunSucceeded, c.Reason
		case batchv1.JobFailed:
			return opsv1alpha1.RunFailed, c.Reason
		}
	}
	return opsv1alpha1.RunRunning, ""
}

// history renders the observation as the status field, newest slot first. Its
// length is bounded by retention, because retention bounds how many Jobs
// exist.
func (o observation) history() []opsv1alpha1.RunRecord {
	if len(o.runs) == 0 {
		return nil
	}
	out := make([]opsv1alpha1.RunRecord, 0, len(o.runs))
	for _, r := range o.runs {
		rec := opsv1alpha1.RunRecord{
			JobName: r.job.Name,
			Slot:    metav1.NewTime(r.slot),
			Phase:   r.phase,
			Reason:  r.reason,
		}
		if r.job.Status.StartTime != nil {
			rec.StartTime = r.job.Status.StartTime.DeepCopy()
		}
		// A failed Job has no completionTime. Reading one from a failure is
		// how a "last successful backup" field ends up reporting a failure.
		if r.phase == opsv1alpha1.RunSucceeded && r.job.Status.CompletionTime != nil {
			rec.CompletionTime = r.job.Status.CompletionTime.DeepCopy()
		}
		out = append(out, rec)
	}
	return out
}

// activeNames is the status field, in slot order.
func (o observation) activeNames() []string {
	if len(o.active) == 0 {
		return nil
	}
	out := make([]string, 0, len(o.active))
	for _, r := range o.active {
		out = append(out, r.job.Name)
	}
	return out
}

// lastSuccessful is the newest completion time still present in the cluster.
// It moves backwards when retention deletes the Job it came from, which is
// the honest answer to "what can I still inspect".
func (o observation) lastSuccessful() *metav1.Time {
	var best *metav1.Time
	for i := range o.succeeded {
		ct := o.succeeded[i].job.Status.CompletionTime
		if ct == nil {
			continue
		}
		if best == nil || ct.After(best.Time) {
			best = ct
		}
	}
	return best.DeepCopy()
}

// prunable returns the Jobs retention should delete: everything past
// keepSucceeded among the successes and past keepFailed among the failures.
// Active Jobs are never in the list.
func (o observation) prunable(keepSucceeded, keepFailed int32) []*batchv1.Job {
	var out []*batchv1.Job
	for i := int(keepSucceeded); i < len(o.succeeded); i++ {
		out = append(out, o.succeeded[i].job)
	}
	for i := int(keepFailed); i < len(o.failed); i++ {
		out = append(out, o.failed[i].job)
	}
	return out
}
