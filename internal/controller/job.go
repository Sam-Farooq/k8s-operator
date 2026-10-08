package controller

import (
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

// mountPath is where the destination claim is mounted inside the dump
// container. It is fixed rather than configurable because the only thing a
// second path would change is the length of this file.
const mountPath = "/backups"

// slotLayout is the slot's wire form. It is used in the Job name, in a label
// value and in the dump file name, so it has to be valid in all three: no
// colons, no plus signs, and sortable as a string.
const slotLayout = "20060102T150405Z"

// nameSlotLayout is the shorter form used in the Job name. Two slots inside
// one minute cannot exist, so minute precision is enough to be unique and it
// leaves the name readable in kubectl output.
const nameSlotLayout = "20060102-1504"

// dumpScript is a constant. Every value that varies between PostgresBackups
// reaches it through the environment, which means no field from the spec is
// ever concatenated into a shell command.
//
// Order matters twice here. The dump writes to a .part path and is renamed
// only after pg_dump exits zero, so a killed dump never leaves a file that
// looks like a backup. And the prune sits after the rename under `set -e`, so
// a failed dump deletes nothing.
const dumpScript = `set -eu
umask 077

dir="$BACKUP_DIR"
mkdir -p "$dir"
out="$dir/$BACKUP_PREFIX-$BACKUP_SLOT.dump"

rm -f -- "$out.part"
pg_dump --format=custom --compress=6 --no-owner --no-privileges --file="$out.part"
mv -- "$out.part" "$out"
echo "wrote $out"

if [ "$BACKUP_PRUNE" = "true" ]; then
	# Dump file names are built by the operator from a pattern-validated
	# prefix and a timestamp, so there is nothing in them for ls to mangle.
	ls -1t -- "$dir/$BACKUP_PREFIX"-*.dump | tail -n +"$((BACKUP_KEEP + 1))" | while IFS= read -r stale; do
		echo "pruning $stale"
		rm -f -- "$stale"
	done
	# A .part file older than the longest a run is allowed to last cannot
	# belong to a run that is still going, so it is a dump that was killed.
	find "$dir" -maxdepth 1 -name "$BACKUP_PREFIX-*.dump.part" -mmin "+$BACKUP_STALE_MINUTES" -delete
fi
`

// JobName derives the Job name for a slot. It is a pure function of the owner
// and the slot, which is the whole idempotency mechanism: a reconcile that
// runs twice for the same slot computes the same name, and the second Create
// comes back AlreadyExists instead of starting a second dump.
//
// A PostgresBackup name longer than 49 characters is truncated, so two long
// names sharing a prefix produce the same Job name. The reconciler checks the
// owner reference on an AlreadyExists and reports a collision rather than
// adopting someone else's Job.
func JobName(owner string, slot time.Time) string {
	suffix := slot.UTC().Format(nameSlotLayout)
	maxOwner := validation.DNS1123LabelMaxLength - len(suffix) - 1
	if len(owner) > maxOwner {
		owner = owner[:maxOwner]
	}
	return fmt.Sprintf("%s-%s", owner, suffix)
}

// SlotLabel is the slot as a label value, used to find the Job for a slot
// without parsing its name.
func SlotLabel(slot time.Time) string { return slot.UTC().Format(slotLayout) }

// BuildJob renders the Job for one slot. It does not set an owner reference:
// that needs the scheme and belongs to the reconciler, which is also the only
// place that can be held responsible for getting it wrong.
func BuildJob(pb *opsv1alpha1.PostgresBackup, slot time.Time) *batchv1.Job {
	spec := pb.Spec
	dir := mountPath
	if spec.Destination.SubPath != "" {
		dir = mountPath + "/" + spec.Destination.SubPath
	}

	prune := spec.Retention.PruneFilesOrDefault()

	// A .part file cannot outlive activeDeadlineSeconds, because the Job is
	// killed at it. Two extra minutes covers the kubelet's own termination
	// grace period and the minute the mtime comparison rounds to.
	staleMinutes := spec.ActiveDeadlineSeconds/60 + 2

	labels := map[string]string{
		opsv1alpha1.LabelOwner: pb.Name,
		opsv1alpha1.LabelSlot:  SlotLabel(slot),
	}
	annotations := map[string]string{
		opsv1alpha1.AnnotationRFC: slot.UTC().Format(time.RFC3339),
	}

	backoff := spec.BackoffLimitOrDefault()
	deadline := spec.ActiveDeadlineSeconds
	one := int32(1)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        JobName(pb.Name, slot),
			Namespace:   pb.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: batchv1.JobSpec{
			Parallelism: &one,
			Completions: &one,
			// No TTLSecondsAfterFinished. The controller reads each finished
			// Job to build status.history and status.lastSuccessfulTime, so a
			// TTL would delete the evidence on a timer the controller does not
			// control. Retention in the spec owns that deletion instead.
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					// Never, not OnFailure. On failure the kubelet keeps the
					// pod and the Job controller creates a new one, so the
					// logs of the attempt that failed are still there to read.
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccountName,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:    "dump",
						Image:   spec.Image,
						Command: []string{"/bin/sh", "-c", dumpScript},
						Env:     dumpEnv(pb, slot, dir, prune, staleMinutes),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "backups", MountPath: mountPath},
							{Name: "tmp", MountPath: "/tmp"},
						},
						Resources: spec.Resources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "backups",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: spec.Destination.ClaimName,
								},
							},
						},
						{
							// readOnlyRootFilesystem is only usable because
							// this exists: pg_dump writes to TMPDIR while it
							// builds the custom format archive.
							Name:         "tmp",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

// dumpEnv builds the container environment. The password is a secretKeyRef
// and the username is too: neither appears in the Job spec, so neither shows
// up in `kubectl get job -o yaml` or in the pod's command line.
func dumpEnv(pb *opsv1alpha1.PostgresBackup, slot time.Time, dir string, prune bool, staleMinutes int64) []corev1.EnvVar {
	src := pb.Spec.Source
	secret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: src.SecretName},
				Key:                  key,
			},
		}
	}
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: src.Host},
		{Name: "PGPORT", Value: strconv.Itoa(int(src.Port))},
		{Name: "PGDATABASE", Value: src.Database},
		{Name: "PGSSLMODE", Value: src.SSLMode},
		{Name: "PGUSER", ValueFrom: secret(src.UsernameKey)},
		{Name: "PGPASSWORD", ValueFrom: secret(src.PasswordKey)},
		{Name: "PGCONNECT_TIMEOUT", Value: "10"},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "BACKUP_DIR", Value: dir},
		{Name: "BACKUP_PREFIX", Value: pb.Spec.Destination.Prefix},
		{Name: "BACKUP_SLOT", Value: SlotLabel(slot)},
		{Name: "BACKUP_PRUNE", Value: strconv.FormatBool(prune)},
		{Name: "BACKUP_KEEP", Value: strconv.Itoa(int(pb.Spec.Retention.KeepSucceededOrDefault()))},
		{Name: "BACKUP_STALE_MINUTES", Value: strconv.FormatInt(staleMinutes, 10)},
	}
}

func ptr[T any](v T) *T { return &v }
