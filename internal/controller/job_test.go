package controller

import (
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	opsv1alpha1 "github.com/Sam-Farooq/k8s-operator/api/v1alpha1"
)

func slot(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func sample() *opsv1alpha1.PostgresBackup {
	return &opsv1alpha1.PostgresBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-nightly", Namespace: "data"},
		Spec: opsv1alpha1.PostgresBackupSpec{
			Schedule: "30 2 * * *",
			Image:    "postgres:17.2-alpine",
			Source: opsv1alpha1.SourceSpec{
				Host:        "orders-primary",
				Port:        5432,
				Database:    "orders",
				SecretName:  "orders-backup-role",
				UsernameKey: "username",
				PasswordKey: "password",
				SSLMode:     "require",
			},
			Destination: opsv1alpha1.DestinationSpec{
				ClaimName: "pg-backups",
				Prefix:    "orders",
			},
			Retention: opsv1alpha1.RetentionSpec{
				KeepSucceeded: 7,
				KeepFailed:    ptr(int32(3)),
			},
			ConcurrencyPolicy:       opsv1alpha1.ConcurrencyForbid,
			StartingDeadlineSeconds: 600,
			ActiveDeadlineSeconds:   3600,
			BackoffLimit:            ptr(int32(2)),
			CleanupTimeoutSeconds:   ptr(int64(900)),
		},
	}
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	out := make(map[string]corev1.EnvVar, len(c.Env))
	for _, e := range c.Env {
		out[e.Name] = e
	}
	return out
}

func TestJobNameIsDeterministic(t *testing.T) {
	s := slot("2026-10-08T02:30:00Z")
	first := JobName("orders-nightly", s)
	second := JobName("orders-nightly", s.Add(45*time.Second))
	if first != second {
		t.Fatalf("JobName moved with sub-minute drift: %q then %q", first, second)
	}
	if first != "orders-nightly-20261008-0230" {
		t.Fatalf("JobName = %q", first)
	}
}

func TestJobNameFitsALabel(t *testing.T) {
	long := strings.Repeat("a", 253)
	name := JobName(long, slot("2026-10-08T02:30:00Z"))
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Fatalf("JobName(%d chars) = %q, not a valid label: %v", len(long), name, errs)
	}
	if len(name) != validation.DNS1123LabelMaxLength {
		t.Fatalf("truncated name is %d chars, want %d", len(name), validation.DNS1123LabelMaxLength)
	}
}

func TestJobNameTruncationCanCollide(t *testing.T) {
	// This is a documented limitation, not an accident, and the reconciler
	// detects it by checking the owner reference. The test pins the fact so
	// nobody removes that check believing it is dead code.
	s := slot("2026-10-08T02:30:00Z")
	a := JobName(strings.Repeat("x", 49)+"-alpha", s)
	b := JobName(strings.Repeat("x", 49)+"-beta", s)
	if a != b {
		t.Fatalf("expected the two 49 character prefixes to collide, got %q and %q", a, b)
	}
}

func TestJobNameUsesMinutePrecision(t *testing.T) {
	a := JobName("db", slot("2026-10-08T02:30:00Z"))
	b := JobName("db", slot("2026-10-08T02:31:00Z"))
	if a == b {
		t.Fatal("two slots a minute apart produced the same Job name")
	}
}

func TestBuildJobShape(t *testing.T) {
	pb := sample()
	job := BuildJob(pb, slot("2026-10-08T02:30:00Z"))

	if job.Namespace != "data" {
		t.Errorf("namespace = %q, want data", job.Namespace)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Error("TTLSecondsAfterFinished is set, which would delete the Job the controller reads status from")
	}
	if got := job.Spec.Template.Spec.RestartPolicy; got != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never so a failed attempt keeps its pod", got)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 2 {
		t.Error("backoffLimit did not come from the spec")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 3600 {
		t.Error("activeDeadlineSeconds did not come from the spec")
	}
	if job.Labels[opsv1alpha1.LabelOwner] != "orders-nightly" {
		t.Errorf("owner label = %q", job.Labels[opsv1alpha1.LabelOwner])
	}
	if job.Labels[opsv1alpha1.LabelSlot] != "20261008T023000Z" {
		t.Errorf("slot label = %q", job.Labels[opsv1alpha1.LabelSlot])
	}
	if errs := validation.IsValidLabelValue(job.Labels[opsv1alpha1.LabelSlot]); len(errs) > 0 {
		t.Errorf("slot label is not a valid label value: %v", errs)
	}
	if got := job.Annotations[opsv1alpha1.AnnotationRFC]; got != "2026-10-08T02:30:00Z" {
		t.Errorf("slot annotation = %q", got)
	}
	// The pod template carries the same labels, so `kubectl logs -l` finds
	// the pod and not only the Job.
	if job.Spec.Template.Labels[opsv1alpha1.LabelSlot] != job.Labels[opsv1alpha1.LabelSlot] {
		t.Error("pod template is missing the slot label")
	}
}

func TestBuildJobKeepsCredentialsOutOfTheSpec(t *testing.T) {
	pb := sample()
	job := BuildJob(pb, slot("2026-10-08T02:30:00Z"))
	env := envOf(job.Spec.Template.Spec.Containers[0])

	for _, name := range []string{"PGUSER", "PGPASSWORD"} {
		e, ok := env[name]
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		if e.Value != "" {
			t.Errorf("%s has a literal value in the Job spec", name)
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s is not a secretKeyRef", name)
		}
		if e.ValueFrom.SecretKeyRef.Name != "orders-backup-role" {
			t.Errorf("%s reads secret %q", name, e.ValueFrom.SecretKeyRef.Name)
		}
	}
	if env["PGUSER"].ValueFrom.SecretKeyRef.Key != "username" {
		t.Error("PGUSER did not use spec.source.usernameKey")
	}
	if env["PGPASSWORD"].ValueFrom.SecretKeyRef.Key != "password" {
		t.Error("PGPASSWORD did not use spec.source.passwordKey")
	}
	// pg_dump takes the database from PGDATABASE, so no connection string
	// reaches the command line where `ps` would show it.
	for _, arg := range job.Spec.Template.Spec.Containers[0].Command {
		if strings.Contains(arg, "orders-backup-role") || strings.Contains(arg, "password") {
			t.Errorf("command carries credential material: %q", arg)
		}
	}
}

func TestBuildJobConnectionEnv(t *testing.T) {
	pb := sample()
	pb.Spec.Source.Port = 6432
	pb.Spec.Source.SSLMode = "verify-full"
	env := envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])

	want := map[string]string{
		"PGHOST":     "orders-primary",
		"PGPORT":     "6432",
		"PGDATABASE": "orders",
		"PGSSLMODE":  "verify-full",
		"TMPDIR":     "/tmp",
	}
	for k, v := range want {
		if env[k].Value != v {
			t.Errorf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
}

func TestBuildJobDestinationDirectory(t *testing.T) {
	pb := sample()
	env := envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])
	if env["BACKUP_DIR"].Value != "/backups" {
		t.Errorf("BACKUP_DIR with no subPath = %q", env["BACKUP_DIR"].Value)
	}

	pb.Spec.Destination.SubPath = "orders/daily"
	env = envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])
	if env["BACKUP_DIR"].Value != "/backups/orders/daily" {
		t.Errorf("BACKUP_DIR with a subPath = %q", env["BACKUP_DIR"].Value)
	}
}

func TestBuildJobPruneDefaultsOn(t *testing.T) {
	pb := sample()
	if pb.Spec.Retention.PruneFiles != nil {
		t.Fatal("the fixture should leave pruneFiles unset")
	}
	env := envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])
	if env["BACKUP_PRUNE"].Value != "true" {
		t.Errorf("BACKUP_PRUNE with pruneFiles unset = %q, want true", env["BACKUP_PRUNE"].Value)
	}

	off := false
	pb.Spec.Retention.PruneFiles = &off
	env = envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])
	if env["BACKUP_PRUNE"].Value != "false" {
		t.Errorf("BACKUP_PRUNE with pruneFiles false = %q, want false", env["BACKUP_PRUNE"].Value)
	}
}

func TestBuildJobStaleMinutesExceedTheDeadline(t *testing.T) {
	// The .part sweep must never be able to delete a file belonging to a run
	// that is still going, so its threshold has to sit above the longest a
	// run may last.
	for _, deadline := range []int64{60, 600, 3600, 86400} {
		pb := sample()
		pb.Spec.ActiveDeadlineSeconds = deadline
		env := envOf(BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0])
		got := env["BACKUP_STALE_MINUTES"].Value
		mins, err := strconv.ParseInt(got, 10, 64)
		if err != nil {
			t.Fatalf("BACKUP_STALE_MINUTES = %q: %v", got, err)
		}
		if mins*60 <= deadline {
			t.Errorf("deadline %ds gives a %dm sweep threshold, which can hit a live run", deadline, mins)
		}
	}
}

func TestBuildJobVolumesAndSecurity(t *testing.T) {
	job := BuildJob(sample(), slot("2026-10-08T02:30:00Z"))
	pod := job.Spec.Template.Spec

	claim := ""
	hasTmp := false
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
		if v.Name == "tmp" && v.EmptyDir != nil {
			hasTmp = true
		}
	}
	if claim != "pg-backups" {
		t.Errorf("claim = %q, want pg-backups", claim)
	}
	if !hasTmp {
		t.Error("no emptyDir for /tmp, so readOnlyRootFilesystem would break pg_dump")
	}

	c := pod.Containers[0]
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem is not set")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation is not false")
	}
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot is not set")
	}
	mounted := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounted[m.Name] = m.MountPath
	}
	if mounted["backups"] != "/backups" || mounted["tmp"] != "/tmp" {
		t.Errorf("volume mounts = %v", mounted)
	}
}

func TestBuildJobPassesResourcesThrough(t *testing.T) {
	pb := sample()
	pb.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
	c := BuildJob(pb, slot("2026-10-08T02:30:00Z")).Spec.Template.Spec.Containers[0]
	if c.Resources.Limits.Memory().String() != "1Gi" {
		t.Errorf("memory limit = %s", c.Resources.Limits.Memory())
	}
}

func TestDumpScriptOrdering(t *testing.T) {
	dump := strings.Index(dumpScript, "pg_dump")
	rename := strings.Index(dumpScript, `mv -- "$out.part" "$out"`)
	prune := strings.Index(dumpScript, "ls -1t")
	if dump < 0 || rename < 0 || prune < 0 {
		t.Fatal("the script lost one of its three steps")
	}
	if !(dump < rename && rename < prune) {
		t.Fatalf("script order is dump=%d rename=%d prune=%d; a prune before the rename deletes a backup to make room for one that does not exist yet", dump, rename, prune)
	}
	if !strings.HasPrefix(dumpScript, "set -eu\n") {
		t.Error("the script does not start with set -eu, so a failed pg_dump would fall through to the prune")
	}
	if strings.Contains(dumpScript, "$BACKUP_PREFIX-*.dump.part") == false {
		t.Error("the .part sweep is missing")
	}
	// Nothing from the spec may be interpolated into the script.
	for _, leak := range []string{"orders-primary", "pg-backups", "postgres:17"} {
		if strings.Contains(dumpScript, leak) {
			t.Errorf("the script is not a constant: it contains %q", leak)
		}
	}
}
