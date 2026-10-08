package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConcurrencyPolicy decides what happens when a slot comes round while the
// previous Job is still running.
// +kubebuilder:validation:Enum=Allow;Forbid
type ConcurrencyPolicy string

const (
	// ConcurrencyAllow starts the new Job anyway. Two pg_dump processes on one
	// database double the read load and the WAL pressure, and the dumps land
	// in different files because the file name carries the slot, so nothing
	// is corrupted. It is a capacity decision, not a correctness one.
	ConcurrencyAllow ConcurrencyPolicy = "Allow"

	// ConcurrencyForbid skips the slot and records why. This is the default.
	ConcurrencyForbid ConcurrencyPolicy = "Forbid"
)

// There is deliberately no Replace policy. Replacing means deleting a running
// pg_dump, and a dump killed partway through has already written most of a
// file. The Job writes to a .part path and renames on success precisely so a
// killed dump leaves nothing that looks like a backup, but the next slot would
// then still be racing the old pod's shutdown for the same database. A missed
// slot that says it was missed beats a backup that may or may not exist.

// SourceSpec is the database to dump. Credentials come from a Secret in the
// same namespace; nothing in this API accepts a password.
type SourceSpec struct {
	// Host is the Postgres host, usually a Service name.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port is the Postgres port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	Port int32 `json:"port,omitempty"`

	// Database is the database name passed to pg_dump.
	// +kubebuilder:validation:MinLength=1
	Database string `json:"database"`

	// SecretName is a Secret in this namespace holding the credentials.
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`

	// UsernameKey is the key in that Secret holding the role name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=username
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the key in that Secret holding the password.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=password
	PasswordKey string `json:"passwordKey,omitempty"`

	// SSLMode is passed through to libpq as PGSSLMODE. The default is
	// require, not disable, because a backup is the whole database crossing
	// the network.
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default=require
	SSLMode string `json:"sslMode,omitempty"`
}

// DestinationSpec is where the dump is written. One kind of destination is
// supported: a PersistentVolumeClaim the Job mounts.
type DestinationSpec struct {
	// ClaimName is a PersistentVolumeClaim in this namespace. The operator
	// never mounts it and never reads it.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`

	// Prefix starts every dump file name, so one claim can hold the backups
	// of several databases. It is also what the in-Job prune matches on,
	// which is why it is restricted to the characters a glob treats as
	// literal.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=40
	Prefix string `json:"prefix"`

	// SubPath is a directory inside the claim, created by the Job if absent.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-/a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=120
	SubPath string `json:"subPath,omitempty"`
}

// RetentionSpec has two halves that are enforced in two different places, and
// the split matters: the controller owns Kubernetes objects, the Job owns
// files.
type RetentionSpec struct {
	// KeepSucceeded is how many successful runs keep their Job object and
	// their entry in status.history. It is also the file count the in-Job
	// prune keeps.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=7
	KeepSucceeded int32 `json:"keepSucceeded,omitempty"`

	// KeepFailed is how many failed Jobs are kept for their logs. Zero means
	// a failure is gone before anyone reads it, which is a thing somebody
	// will want, so this is a pointer.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=3
	KeepFailed *int32 `json:"keepFailed,omitempty"`

	// PruneFiles lets the Job delete older dump files in its own prefix after
	// a successful dump.
	//
	// It is a pointer for the reason every pointer in this spec is one: with
	// omitempty, a zero value marshals as absent, the API server applies the
	// default, and the value comes back as its opposite. Every field whose
	// zero is both legal and different from its default is a pointer here.
	// The fields with a Minimum above zero are not, because for those there
	// is no zero to express.
	// +kubebuilder:default=true
	PruneFiles *bool `json:"pruneFiles,omitempty"`
}

// PostgresBackupSpec is the desired state of a PostgresBackup.
type PostgresBackupSpec struct {
	// Schedule is a five field cron expression evaluated in UTC. Names and
	// @daily style macros are rejected.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=200
	Schedule string `json:"schedule"`

	// Source is the database to dump.
	Source SourceSpec `json:"source"`

	// Destination is where the dump goes.
	Destination DestinationSpec `json:"destination"`

	// Retention decides what is deleted and when.
	Retention RetentionSpec `json:"retention,omitempty"`

	// Image must contain pg_dump and a shell. Its major version has to be at
	// or above the server's, because pg_dump refuses a newer server and
	// nothing in this operator can tell you that before the first run fails.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// ConcurrencyPolicy decides what a slot does when a Job is already
	// running.
	// +kubebuilder:default=Forbid
	ConcurrencyPolicy ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds is how late a slot may be started. A slot older
	// than this is counted in status.skippedSlots and never run, which is
	// what stops a restart after a long outage from creating a Job for every
	// slot it missed.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=600
	StartingDeadlineSeconds int64 `json:"startingDeadlineSeconds,omitempty"`

	// ActiveDeadlineSeconds is passed to the Job. A dump that hangs on a lock
	// has to fail rather than hold the slot forever.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=3600
	ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds,omitempty"`

	// BackoffLimit is passed to the Job. Zero means one attempt and no
	// retry, so this is a pointer.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=2
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// Suspend stops new Jobs being created. A Job already running is left to
	// finish, because killing a dump halfway is the thing this operator will
	// not do.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// CleanupTimeoutSeconds bounds how long deletion waits for owned Jobs to
	// go away before the finalizer is removed regardless. Past it the object
	// is deleted and the Jobs are reported as abandoned rather than the
	// object being stuck forever.
	//
	// Zero means never wait, which is the setting for a cluster where
	// nobody is going to come and unstick a volume, so this is a pointer.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=900
	CleanupTimeoutSeconds *int64 `json:"cleanupTimeoutSeconds,omitempty"`

	// ServiceAccountName is the account the Job's pod runs as.
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Resources is the dump container's requests and limits. A Job with no
	// memory limit on a shared node is how a backup takes down the thing it
	// is backing up.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// RunPhase is where one run ended up.
// +kubebuilder:validation:Enum=Running;Succeeded;Failed
type RunPhase string

const (
	// RunRunning means the Job exists and has reported neither Complete nor
	// Failed.
	RunRunning RunPhase = "Running"
	// RunSucceeded means the Job reported Complete.
	RunSucceeded RunPhase = "Succeeded"
	// RunFailed means the Job reported Failed.
	RunFailed RunPhase = "Failed"
)

// RunRecord is one slot's outcome. Every field is read back off the Job on
// each reconcile rather than written once, so deleting a Job by hand removes
// its record and nothing goes out of step.
type RunRecord struct {
	// JobName is the Job this record was read from.
	JobName string `json:"jobName"`

	// Slot is the scheduled time the Job was created for, which is also what
	// its name is derived from.
	Slot metav1.Time `json:"slot"`

	// Phase is where the run ended up.
	Phase RunPhase `json:"phase"`

	// StartTime is the Job's own start time.
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is the Job's own completion time, absent while running
	// and absent on failure, because a failed Job has no completion time.
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Reason is the failed Job condition's reason, when there is one.
	Reason string `json:"reason,omitempty"`
}

// PostgresBackupStatus is the observed state of a PostgresBackup.
type PostgresBackupStatus struct {
	// ObservedGeneration is the spec generation this status was computed
	// from. A status whose observedGeneration is behind metadata.generation
	// describes an older spec and should not be trusted.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the Kubernetes convention: Ready, Running and
	// Degraded, each carrying the generation it was set from.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastScheduleTime is the most recent slot the controller has acted on,
	// whether that action was creating a Job or deciding the slot was too
	// late to run.
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is the completion time of the newest Job that
	// succeeded and is still present. It goes backwards when retention
	// deletes that Job, which is correct: the field describes what can still
	// be inspected.
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// NextScheduleTime is when the next slot falls, recomputed every
	// reconcile.
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// ActiveJobs are the owned Jobs that have not finished.
	// +listType=atomic
	// +optional
	ActiveJobs []string `json:"activeJobs,omitempty"`

	// History is one record per owned Job, newest slot first.
	// +listType=atomic
	// +optional
	History []RunRecord `json:"history,omitempty"`

	// SkippedSlots counts slots that were past the starting deadline by the
	// time the controller looked. It only ever increases.
	SkippedSlots int32 `json:"skippedSlots,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pgbackup;pgb,categories=ops
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last Success",type=date,JSONPath=`.status.lastSuccessfulTime`
// +kubebuilder:printcolumn:name="Next",type=string,JSONPath=`.status.nextScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PostgresBackup is a scheduled logical backup of one Postgres database.
type PostgresBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresBackupSpec   `json:"spec,omitempty"`
	Status PostgresBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PostgresBackupList is a list of PostgresBackup.
type PostgresBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresBackup{}, &PostgresBackupList{})
}

// Condition types set on a PostgresBackup.
const (
	// ConditionReady is true when the schedule parses and the most recent
	// finished run succeeded. It is the one condition a human should read
	// first.
	ConditionReady = "Ready"

	// ConditionRunning is true while an owned Job has not finished.
	ConditionRunning = "Running"

	// ConditionDegraded is an abnormal-true condition: present and true only
	// when something needs attention.
	ConditionDegraded = "Degraded"
)

// Condition reasons. Kubernetes requires a reason on every condition and
// requires it to be a CamelCase token, so they are constants rather than
// sentences assembled at the call site.
const (
	ReasonScheduled            = "Scheduled"
	ReasonSuspended            = "Suspended"
	ReasonInvalidSchedule      = "InvalidSchedule"
	ReasonJobRunning           = "JobRunning"
	ReasonNoJobRunning         = "NoJobRunning"
	ReasonLastRunSucceeded     = "LastRunSucceeded"
	ReasonLastRunFailed        = "LastRunFailed"
	ReasonNoRunsYet            = "NoRunsYet"
	ReasonDeadlineExceeded     = "StartingDeadlineExceeded"
	ReasonConcurrencyForbidden = "ConcurrencyForbidden"
	ReasonHealthy              = "Healthy"
	ReasonJobNameCollision     = "JobNameCollision"
	ReasonCleanupAbandoned     = "CleanupAbandoned"
	ReasonCleanupInProgress    = "CleanupInProgress"
)

// Finalizer is removed only once owned Jobs are gone, or once
// spec.cleanupTimeoutSeconds has passed since deletion was requested.
const Finalizer = "ops.sam-farooq.dev/backup-cleanup"

// Labels the controller puts on every Job it creates, and the annotation that
// carries the slot in a form a human can read.
const (
	LabelOwner    = "ops.sam-farooq.dev/postgresbackup"
	LabelSlot     = "ops.sam-farooq.dev/slot"
	AnnotationRFC = "ops.sam-farooq.dev/slot-rfc3339"
)

// The defaults below exist twice: once as a kubebuilder marker, which the API
// server applies, and once here, which is what a reconciler sees when it is
// handed an object that did not come through the API server. A test reads the
// generated CRD and fails when the two disagree.
const (
	DefaultBackoffLimit          int32 = 2
	DefaultKeepFailed            int32 = 3
	DefaultKeepSucceeded         int32 = 7
	DefaultCleanupTimeoutSeconds int64 = 900
	DefaultPruneFiles                  = true
)

// BackoffLimitOrDefault is spec.backoffLimit with the default applied.
func (s *PostgresBackupSpec) BackoffLimitOrDefault() int32 {
	if s.BackoffLimit == nil {
		return DefaultBackoffLimit
	}
	return *s.BackoffLimit
}

// CleanupTimeout is spec.cleanupTimeoutSeconds as a duration, with the
// default applied.
func (s *PostgresBackupSpec) CleanupTimeout() time.Duration {
	if s.CleanupTimeoutSeconds == nil {
		return time.Duration(DefaultCleanupTimeoutSeconds) * time.Second
	}
	return time.Duration(*s.CleanupTimeoutSeconds) * time.Second
}

// KeepFailedOrDefault is retention.keepFailed with the default applied.
func (r *RetentionSpec) KeepFailedOrDefault() int32 {
	if r.KeepFailed == nil {
		return DefaultKeepFailed
	}
	return *r.KeepFailed
}

// KeepSucceededOrDefault is retention.keepSucceeded with the default applied.
// It is not a pointer, because its minimum is one and there is no zero to
// tell apart from absent.
func (r *RetentionSpec) KeepSucceededOrDefault() int32 {
	if r.KeepSucceeded == 0 {
		return DefaultKeepSucceeded
	}
	return r.KeepSucceeded
}

// PruneFilesOrDefault is retention.pruneFiles with the default applied.
func (r *RetentionSpec) PruneFilesOrDefault() bool {
	if r.PruneFiles == nil {
		return DefaultPruneFiles
	}
	return *r.PruneFiles
}
