# k8s-operator

`eks-infra` builds a cluster and `gitops-deploy` deploys onto it. Neither adds
anything to the Kubernetes API. This does: one CRD, `PostgresBackup`, and the
controller behind it, which turns a cron expression and a retention policy
into Jobs that run `pg_dump`.

The CRD is the small part. The reconciler is the point, because a reconciler
is not a function that runs when something happens. It is a function that is
called again, for reasons it is not told, in an order it cannot rely on, and
it has to be correct on the first call after a restart as well as on the
thousandth. Every decision in `internal/controller` is derived from the spec,
the clock, and the Jobs the API server currently reports. Nothing is derived
from what the previous call did.

```
api/v1alpha1/            the types, the markers, the defaults, the conditions
internal/schedule/       five field cron, UTC, with Latest as well as Next
internal/controller/     decide(), observe(), the reconciler, the finalizer
config/crd/bases/        486 lines of CRD, generated from the types
config/rbac/             the ClusterRole, generated from the markers
config/samples/          two objects the test suite applies for real
cmd/                     the manager
docs/restoring.md        how a dump is read back, and what has not been tested
```

85 tests. 20 of them drive a real `kube-apiserver` and `etcd` through envtest,
and none of them needs a cluster, a credential or a network connection once
the control plane binaries are cached. 2,060 lines of hand written Go, 225
generated, 2,478 of tests.

Nothing here has ever run against a Postgres server. Every number below is
computable from this repo as it stands, or is labelled as a bound that was
chosen.

## Idempotency is a naming decision

`Reconcile` may be called twice for the same state, and on the second call it
must not start a second dump. The mechanism is that the Job's name is a pure
function of the owner and the slot:

```
orders-nightly-20261009-0230
```

So the second call builds the same name, `Create` returns `AlreadyExists`, and
nothing happens. There is no "did I already do this" flag anywhere, which
means there is no flag to be wrong after a crash between the create and the
status write.

`AlreadyExists` has two causes and they are not the same thing. Either this
reconcile raced another copy of itself, which is the mechanism working, or
something else holds that name. The controller reads the existing Job and
compares the controller owner reference's UID. If it is ours, the slot is
done. If it is not, the slot is **not run**: `Degraded` goes true with reason
`JobNameCollision`, `lastScheduleTime` is deliberately left where it was, and
the operator does not adopt a Job it did not create.

That collision is reachable. A `PostgresBackup` name is truncated to 49
characters to leave room for the timestamp inside the 63 character label
limit, so two long names sharing a prefix derive the same Job name.
`TestJobNameTruncationCanCollide` pins the fact so the check does not get
deleted as dead code.

## What happens when two reconciles overlap

**Two reconciles of the same object, in one manager: they cannot.**
controller-runtime keys the workqueue by object, holds one key in flight at a
time, and `MaxConcurrentReconciles` is 1 unless it is changed. Events that
arrive while a reconcile is running coalesce into one later call. Two
different `PostgresBackup` objects do reconcile at once, and they share
nothing: separate namespaces of Jobs, separate status.

**Two managers: nothing stops them except leader election**, and
`--leader-elect` defaults to false, which is the wrong setting the moment
there is a second replica. What actually happens without it is worth knowing,
because it is not what people expect:

- Both compute the same slot and therefore the same Job name. One `Create`
  wins, the other gets `AlreadyExists`, reads the owner reference, finds its
  own UID and treats the slot as done. **No second dump starts.** The naming
  decision is what makes that true, not the lease.
- Both then write status. Each write makes the other's copy stale, both get a
  conflict, both requeue, and the object's `resourceVersion` climbs with
  nothing changing in it. That is the real cost: write amplification and a
  status that flaps between two nearly identical versions.
- Both run retention. The delete carries a UID precondition, so the loser gets
  `Conflict` rather than deleting a Job that was recreated under the same name
  in between. That path is handled and not an error.

So the lease is required for correctness of the status, and the name
derivation is what keeps a split brain from dumping twice. Running two
replicas with `--leader-elect=false` is a supported way to produce a lot of
writes and no corruption.

**A reconcile overlapping the Job it just created** is the common case and the
reason `observe()` exists. The created Job is folded into the observation
in-process rather than re-listed, because under a manager the list is served
from a cache that has not seen the create yet. Re-listing is the obvious way
and the wrong one.

## The status is recomputed, not appended to

`statusFrom` builds the whole status from three things: the observation, the
decision, and the next slot. `status.history` is one record per owned Job,
read off the Jobs every time. Delete a Job by hand and its record disappears;
wipe the status entirely and the next reconcile rebuilds it.

Two fields read the old status, and both are stated in the code because they
are the exceptions:

- `lastScheduleTime` only moves forward. It is the floor for the next slot
  decision, and a floor that can go backwards runs a slot twice.
- `skippedSlots` only counts up.

A status write happens only when something in the status moved
(`equality.Semantic.DeepEqual`). This is not an optimisation. The controller
watches the object, so an unconditional write is a reconcile loop that never
stops. It is also the thing that makes the idempotency test possible:
`TestReconcileIsIdempotent` settles, then calls `Reconcile` three more times
with the clock held still, and asserts the Job's UID and `resourceVersion` and
the `PostgresBackup`'s `resourceVersion` never move. Three more calls, not
one, because a pair of alternating states passes a single repeat.

Every condition carries `observedGeneration`, and `status.observedGeneration`
carries the generation the whole status was computed from. A status behind
`metadata.generation` describes an older spec, and that field is the only way
a reader can tell. `TestReconcileStaleStatusIsDetectable` changes the spec and
asserts the conditions report the old generation until the controller has
looked.

## What the conditions mean

| Condition | True when | Notes |
|---|---|---|
| `Ready` | the newest finished run succeeded | `Unknown` before anything has finished. `False` while suspended, on purpose |
| `Running` | an owned Job has not finished | names the Jobs in its message |
| `Degraded` | the newest finished run failed, a Job name collided, or cleanup was abandoned | always present, true only when something wants attention |

`Ready` describes the newest **finished** run, not the newest run. A nightly
failure has to stay visible while tonight's dump is in flight, or it is
invisible for as long as a dump takes.

A suspended backup reports `Ready=False` with reason `Suspended` and
`Degraded=False`. That is a choice. The suspension was asked for, so nothing
is wrong, but a `Ready` that said true while nothing was scheduled is the
field that makes an incident last longer.

**A skipped slot is not a condition.** It is an Event and a tick of
`status.skippedSlots`. The first version raised `Degraded` on a skip, and it
flickered: skipping advances `lastScheduleTime`, so the reconcile two seconds
later had no slot to decide about and set `Degraded` straight back to false.
A condition that is true for one pass is a condition nobody can write an alert
against. A collision stays in `Degraded` precisely because it does not advance
`lastScheduleTime`, so every following reconcile reaches the same conclusion.

## Missed slots, and the restart after an outage

Only the most recent slot is ever considered. A controller that walks the
slots it missed creates a Job for each, and the first reconcile after a
weekend outage then runs a fortnight of backups at once against a live
database.

```
floor   = status.lastScheduleTime, or metadata.creationTimestamp
latest  = schedule.Latest(now)
due     = latest > floor
```

If the latest slot is older than `startingDeadlineSeconds`, it is counted and
not run. The slots in between are counted too, bounded at 1,000, which is a
chosen cap: an operator restarted after a fortnight on a five minute schedule
has 4,032 slots behind it, and the exact figure only ever goes into a log line
and a status field.

`creationTimestamp` as the initial floor is what stops a new `PostgresBackup`
with a yearly schedule from immediately running January's slot.

## The finalizer, and the case where it gives up

The owner reference already makes the garbage collector delete these Jobs, so
the finalizer is not there to free objects. It is there because garbage
collection is asynchronous: `kubectl delete postgresbackup` returns, the
object is gone, and the pod carries on writing into the destination claim. The
finalizer makes the delete wait until nothing is still dumping.

Then there is the case that makes finalizers notorious. A pod can sit
`Terminating` indefinitely, and an unresponsive volume is the usual way it
happens for exactly this workload, where the volume is the backup target.
Nothing the operator can do from inside fixes that. So:

- it asks for every owned Job to be deleted, with background propagation and a
  UID precondition, and does not stop on the first one that refuses
- it requeues on a backoff derived from how long deletion has been pending,
  doubling from 1 second to a 30 second ceiling. Derived from the elapsed time
  rather than held in a counter, so it survives a controller restart. There is
  no sleep anywhere in this repo
- after `spec.cleanupTimeoutSeconds`, default 900, it writes `Degraded` with
  reason `CleanupAbandoned`, emits an event naming every Job it is leaving
  behind, and removes the finalizer anyway

The order of those last three matters: removing the finalizer deletes the
object and the condition with it, so the status write goes first and the event
goes second. The event is the only durable record, for as long as the cluster
keeps events. An object that cannot be deleted is worse than a Job that
outlives it.

`cleanupTimeoutSeconds: 0` means never wait, which is the setting for a
cluster where nobody is coming to unstick a volume.
`TestFinalizerGivesUpOnAJobThatWillNotDelete` holds a Job with a finalizer of
its own and asserts the `PostgresBackup` goes anyway.

## Three fields that could not express zero

`retention.pruneFiles` was a `*bool` from the start, with the reason written
next to it: a field with `omitempty` and a non-zero default marshals its zero
as absent, the API server applies the default, and the value comes back as the
opposite of what was asked for. The reason applied to three more fields and
had not been acted on:

| Field | What zero means | What it became |
|---|---|---|
| `backoffLimit` | one attempt, no retry | 2 |
| `retention.keepFailed` | keep no failures | 3 |
| `cleanupTimeoutSeconds` | never wait on a stuck Job | 900 |

The last one is the one that matters, because it is the escape hatch above,
and it was unreachable. All three are pointers now with accessors carrying the
fallback, and the rule is stated once: a pointer exactly where zero is both
legal and different from the default. `startingDeadlineSeconds` and
`activeDeadlineSeconds` have a minimum of 60 and stay plain, because for those
there is no zero to tell apart from absent.

`api/v1alpha1/defaults_test.go` parses the generated CRD, compares each of the
13 defaults under `spec` against the constant the accessor falls back to, and
counts them, so a default added to a marker and never checked fails the build.
Six more markers are asserted against a real API server in
`TestCRDRejectsWhatTheTypesSayItShould`: the `concurrencyPolicy` and `sslMode`
enums, the `prefix` pattern, both retention maxima, the `startingDeadline`
minimum and the `image` length. A marker is a comment until something refuses
a write because of it.

## The dump script is a constant

Every value that varies between `PostgresBackup` objects reaches the container
through the environment, so no field from the spec is concatenated into a
shell command. `TestDumpScriptOrdering` asserts the script contains none of
the fixture's own values.

```sh
pg_dump --format=custom --compress=6 --no-owner --no-privileges --file="$out.part"
mv -- "$out.part" "$out"
```

The rename is the point. A dump that is killed leaves `.part`, which the prune
does not count and `pg_restore` would not be pointed at. And because the prune
sits after the rename under `set -e`, a failed dump deletes nothing.

`PGUSER` and `PGPASSWORD` are `secretKeyRef`s, so neither appears in
`kubectl get job -o yaml` and neither reaches the pod's command line, where
`ps` would show it. `pg_dump` takes the database from `PGDATABASE`, so there
is no connection string either. **The operator's ClusterRole has no rule for
secrets at all**: it names the Secret in the Job's environment and the kubelet
reads it, so no database password passes through the controller process.

The container runs with `readOnlyRootFilesystem: true`, which is only workable
because an `emptyDir` is mounted at `/tmp` with `TMPDIR` pointed at it:
`pg_dump` writes there while it builds a custom format archive.

`restartPolicy: Never` rather than `OnFailure`, so a failed attempt keeps its
pod and its logs instead of the kubelet restarting the container over them.

There is no `ttlSecondsAfterFinished` on these Jobs, and a test asserts its
absence. The controller reads finished Jobs to build `status.history` and
`lastSuccessfulTime`; a TTL would delete that evidence on a timer the
controller does not own.

## Retention has two halves, in two places

- **Job objects**, by the controller: successes past `keepSucceeded`, failures
  past `keepFailed`. Active Jobs are never candidates. The delete carries the
  Job's UID as a precondition, so a Job recreated under the same name between
  the list and the delete is not the one that goes.
- **Dump files**, by the Job, after its own dump succeeded: `ls -1t` on the
  prefix, keep `keepSucceeded`, delete the rest. The operator never mounts the
  claim and never reads it.

The split is not tidy and the cost is real: **if dumps stop running, old files
stay forever**, because the only thing that prunes files is a successful dump.
That is the right way round for a backup system, where deleting the last good
copy to make room for one that failed is the worst available outcome, and it
means disk use is not bounded by the operator.

The `.part` sweep has an argument behind it rather than a timeout somebody
liked: a `.part` file whose mtime is older than `activeDeadlineSeconds` cannot
belong to a run that is still going, because the Job is killed at that
deadline. The threshold is that plus two minutes, for the pod's termination
grace and the minute `find -mmin` rounds to, and a test asserts the threshold
is above the deadline for every legal deadline value.

## The cron parser, and what it cost

`internal/schedule` is a five field parser, UTC only. Minute, hour, day of
month, month, day of week, with `*`, `a`, `a-b`, `a-b/n`, `*/n` and comma
lists. It answers `Next` and also `Latest`, and `Latest` is the one that makes
the slot decision a function of the clock rather than of what the last
reconcile wrote down.

It implements the rule that catches everyone: when day of month and day of
week are **both** restricted, cron matches a day satisfying **either**.
`0 0 1 3 1` is the first of March and every Monday in March, which is six
days, not one. That is Vixie's behaviour and `TestDayOfMonthAndDayOfWeekAreOred`
asserts all six.

The scan skips a whole day when the day fields miss and a whole hour when the
hour misses, so the iteration count is bounded by days plus hours plus minutes
rather than by minutes alone. Measured on this machine with
`go test -bench . -benchtime 5000x`, three runs:

```
BenchmarkNextOnceAYear            32.1 to 32.6 us/op     "0 0 1 1 *"
BenchmarkNextEveryFiveMinutes      227 to 248  ns/op     "*/5 * * * *"
```

The five year search window is 2,629,800 minutes, which is what a
minute-by-minute walk would cost on the yearly expression. Those are timings
from this laptop and they are the only benchmark numbers in this repo.

**What writing it cost:** 294 lines to maintain, and no `@daily`, no
month names, no weekday names, no seconds field, and no timezone. All five are
rejected with a message rather than silently misread, and 16 expressions are
asserted to be refused. `robfig/cron` does all of it, is used by most
operators that need a schedule, and has had far more eyes on it than 11 tests.
The reason not to take it was one dependency in a controller whose entire
dependency tree is `controller-runtime` and the Kubernetes libraries, and the
reason it is defensible is that the dom/dow rule and the search bound are
visible in this repo instead of being behaviour somebody has to go and read.
If this were a product rather than a repository, the dependency would be the
right call.

## What this operator does not do

1. **It does not verify a dump.** Nothing ever reads the file back, not even
   `pg_restore --list`, which parses the whole archive and would catch a
   truncated one for the cost of a second container. An unverified backup is
   not a backup, and this is the largest gap in the repo.
2. **It does not do point-in-time recovery.** `pg_dump` is a logical dump.
   There is no WAL archiving and no base backup, so the recovery point is the
   last slot, and recovering at 14:03 from an 02:30 nightly loses eleven and a
   half hours. `pgBackRest` and `barman` do this properly and either is the
   right answer where the recovery point is a number somebody has agreed to.
3. **It does not get the dump off the cluster.** The destination is a
   `PersistentVolumeClaim`. No object storage, no retention lock, no second
   copy. A backup on the same storage class as the database it protects
   survives a dropped table and does not survive a lost volume.
4. **It does not back up roles, grants or tablespaces.** `--no-owner
   --no-privileges` is what makes a restore into a scratch database possible,
   and it means the grants are not in the file. That is `pg_dumpall
   --globals-only` territory.
5. **It does not check credentials or versions before running.** It does not
   read the Secret, cannot tell you the role lacks `SELECT`, and does not
   compare the image's `pg_dump` major version against the server's.
   `pg_dump` refuses a newer server, and both failures surface as a failed Job
   with the reason in its logs.
6. **It has no admission webhook.** The CRD's schema is the only validation at
   write time, so an unparseable `schedule` is accepted by the API server and
   reported in status instead. A parse failure returns no error and no
   requeue: retrying it on a timer never produces a different answer, and the
   watch delivers the next spec change without help.
7. **No `Replace` concurrency policy.** Replacing means killing a running
   `pg_dump`. `Forbid` and `Allow` are the two options, `Forbid` is the
   default, and a skipped slot that says it was skipped beats a backup that
   may or may not exist.
8. **No image and no Deployment manifest.** `make build` produces the binary
   and it runs out of cluster against a kubeconfig. Nothing here has built or
   pushed a container image, and a manifest referring to a tag that does not
   exist is worse than its absence.
9. **UTC only.** A schedule that follows a wall clock through a daylight
   saving transition runs twice or not at all on two days a year, and neither
   is worth the field it would take to configure.

## One more tradeoff, stated

`ownedJobs` lists every Job in the namespace and filters on the controller
owner reference. It does not use a label selector and it does not use a field
index on `.metadata.ownerReferences`, which is the idiomatic kubebuilder
pattern and would be cheaper.

The reason is that a label is editable by anyone with `patch` on Jobs. A Job
whose label was removed would become invisible to the reconciler: never
counted for concurrency, never pruned by retention, never cleaned up on
delete. The owner reference is what the garbage collector acts on, so it is
what membership is decided by. Under a manager the list is served from the
cache that `Owns(&batchv1.Job{})` already fills, so the cost is a map walk
rather than a request.

The cost is paid in the tests, which use a direct client, and there it is a
real LIST per reconcile. That was the trade: a direct client makes every read
read-your-writes, which is what turns "the second call changed nothing" from
a race against an informer into an exact assertion on `resourceVersion`. A
field index would have required the cached client and the tests would have had
to poll.

## Running it

```bash
make help              # the targets
make tools             # controller-gen and setup-envtest into ./bin
make generate          # deepcopy functions
make manifests         # the CRD and the ClusterRole, from the markers
make verify-manifests  # fails if the committed generated files are stale
make fmt-check vet     # gofmt and go vet
make lint              # golangci-lint v2.14.0
make envtest           # the control plane binaries, into ./bin/k8s
make test              # 85 tests, control plane included
make test-race         # the same under the race detector
make build             # ./bin/manager
make check             # everything CI runs
```

`make test` sets `KUBEBUILDER_ASSETS` to an absolute path, and it has to.
`go test ./...` runs each package's tests with that package's directory as the
working directory, so a relative assets path resolves differently in every
package and envtest fails with `no such file or directory` on `etcd`.

Against a cluster, with a kubeconfig already pointed at it. This block is the
only one on this page that has not been run from this repo, because there is
no cluster here:

```bash
kubectl apply -f config/crd/bases/
kubectl apply -f config/samples/
./bin/manager --leader-elect --metrics-bind-address=0
kubectl get pgbackup -A
```

The first two lines are not unverified in the way that sounds. `config/crd/bases`
is the exact directory envtest loads into a real `kube-apiserver` 1.32.0 at
the start of the suite, and `TestSamplesApplyCleanly` creates everything in
`config/samples` against it and reads it back through the typed client. What
has not been exercised is the manager's own startup path: `ctrl.NewManager`,
leader election and the signal handler.

`pgbackup` and `pgb` are both short names. The printer columns show the
schedule, whether it is suspended, `Ready`, the last success and the next
slot.

## CI

Three jobs on `ubuntu-24.04`, every action pinned to a tag.

- **lint**: `gofmt -l`, `go vet`, `golangci-lint` v2.14.0 through
  `golangci/golangci-lint-action@v8.0.0`. Eight findings were fixed rather
  than excluded when the linter went in, because a config with a carve-out per
  finding is a config that stops being read. The only exclusion is `revive`
  and `unparam` on `_test.go`, where a helper does not need a doc comment.
- **manifests**: `make verify-manifests`, which regenerates the CRD, the role
  and the deepcopy functions and fails if the committed files move. A CRD
  edited by hand and a CRD generated from the types are indistinguishable
  until the day they disagree.
- **test**: caches `bin/k8s` on the envtest version, which is the only thing
  in the job that touches the network, then builds the manager and runs the
  whole suite under `-race`. After the cache hit the control plane is a local
  `kube-apiserver` and `etcd`, so the suite runs on a pull request from a
  fork.

The Go toolchain comes from `go.mod` through `go-version-file`, so there is
one place to change it.

The envtest suite fails rather than skips when the binaries are missing. A
suite that skips its only integration tests when the assets are absent is a
green tick that means nothing, and CI is exactly where assets go missing.

## Licence

MIT. Version 0.14.2.
