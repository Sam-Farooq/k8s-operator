# Restoring

Nothing in this repo has restored a database. The commands below are the ones
a `--format=custom` dump is restored with, written down because a backup
system that does not say how to read its output is half a system. They have
not been run against a real server from here.

## What the Job writes

One file per slot, in the destination claim, under the prefix and subPath
from the spec:

```
/backups/<subPath>/<prefix>-<YYYYMMDD>T<HHMMSS>Z.dump
```

It is a `pg_dump --format=custom` archive, compressed at level 6, written
without owners and without privileges:

```
pg_dump --format=custom --compress=6 --no-owner --no-privileges
```

`--no-owner` and `--no-privileges` mean the restore does not need the original
role names to exist, which is what makes restoring into a scratch database
possible. It also means the grants are not in the dump, and that is the first
thing to notice: **this is a backup of data and schema, not of who may read
it.** Roles and grants live in `pg_dumpall --globals-only`, which this
operator does not run.

## Reading one without restoring it

```bash
pg_restore --list /backups/orders/daily/orders-20261009T023000Z.dump
```

That prints the table of contents and exits. It parses the archive header and
every entry, so a truncated or corrupt file fails here. It is the cheapest
check that exists, and it is not in the Job, which is the gap named in the
README.

## Restoring into a new database

```bash
createdb orders_restore
pg_restore \
  --dbname=orders_restore \
  --no-owner --no-privileges \
  --jobs=4 \
  --exit-on-error \
  /backups/orders/daily/orders-20261009T023000Z.dump
```

`--exit-on-error` matters. Without it `pg_restore` reports errors and carries
on to the end, exits zero, and leaves a database that is missing whatever
failed. The default is the dangerous one.

`--jobs=4` only works on a custom or directory format archive and only when
restoring to a database rather than to a script. It restores the data and
index steps in parallel.

## Restoring over the live database

There is no flag for this that is safe, and the operator has no opinion about
it because it is not the operator's decision. The shape that works:

1. Restore into a new database beside the old one, as above.
2. Check it: row counts on the tables that matter, and the application's own
   health check pointed at the restored database.
3. Rename, with the application stopped.

`pg_restore --clean --if-exists` drops and recreates each object in place
instead. It is one command, and if it fails partway the live database is now
partly dropped. That is the reason step 1 is a new database.

## What a dump taken while the database was busy contains

`pg_dump` runs in a single repeatable-read transaction, so the dump is
consistent as of its start. There is no partial state and no torn row. What
there is not, is anything committed after it started, and on a large database
"after it started" can be an hour.

It also holds an `ACCESS SHARE` lock on every table for the whole run, which
blocks nothing an application does and does block `ALTER TABLE` and
`DROP TABLE`. A migration that runs at 02:35 against a dump that started at
02:30 waits for it.

## The restore nobody tests

A logical dump has no point-in-time recovery. Recovering to 14:03 when the
nightly ran at 02:30 loses eleven and a half hours, and no setting here
changes that. `pgBackRest` and `barman` do WAL archiving and point-in-time
restore, and either is the right answer for a database where the recovery
point is a number somebody has agreed to. This operator schedules dumps.
