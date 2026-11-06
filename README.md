# ComposeSafe

A safety net for upgrading Docker Compose stacks: assess the risk, take a verified snapshot,
upgrade, verify, and roll back if anything breaks. Commands that change anything default to a dry run.

> Status: **early development (M8)**. `scan`, `plan`, `snapshot`, `upgrade` (including Postgres major versions), `rollback`, `rehearse`, `history`, `report` and `cleanup` work today; the other commands are listed in `--help`
> and report which milestone will deliver them. See [docs/PRD.md](docs/PRD.md) and [docs/DESIGN.md](docs/DESIGN.md).

## Build

```bash
make build        # bin/composesafe (and a bin/csafe symlink)
make check        # vet + tests + gofmt + the "never delete data" guard
```

Requires Go 1.24+, Docker Engine 27+ and Docker Compose 2.24+ on the machine that runs the Docker daemon.

## Try it

```bash
./bin/composesafe scan -f testdata/stacks/toyapp/compose.yml -p toy
./bin/composesafe scan -f testdata/stacks/toyapp/compose.yml -p toy --json
COMPOSESAFE_LANG=zh ./bin/composesafe scan -f testdata/stacks/toyapp/compose.yml -p toy
```

```bash
./bin/composesafe plan db -f compose.yml            # list newer tags of db's image
./bin/composesafe plan db=16.4-alpine -f compose.yml  # assess the upgrade, write report.md
```

`plan` reads release notes (GitHub Releases, or postgresql.org for the official Postgres image), applies the
risk rules and writes `report.md` plus `run.jsonl` under the state directory. It changes nothing in Docker or in your
project. Exit code 2 means a BLOCK-level risk. Set `GITHUB_TOKEN` to raise GitHub's API limit.
If Docker Hub is unreachable, tag listing falls back to the registry mirrors configured on your Docker daemon;
if none can list tags, name the target tag yourself.

```bash
./bin/composesafe snapshot app -f compose.yml                       # dry run: what would be stopped, backed up, how much space
./bin/composesafe snapshot app -f compose.yml --execute             # asks for confirmation; --yes skips the prompt
./bin/composesafe snapshot app -f compose.yml --execute --logical-only   # Postgres dump only, nothing is stopped
```

`snapshot` backs up the named services and the data they depend on: a `pg_dumpall` of Postgres, a cold archive of the
volumes (the data services are stopped for that time and **started again even if the snapshot fails**), the compose
files, `.env`, and a pinned copy of the current images. It then verifies checksums and archive integrity and, unless
`--skip-restore-check` is given, replays the dump into a throwaway server and compares the tables. Everything lands in
`<state dir>/<project>/<run id>/` with mode 0600; that directory holds secrets, treat it like them.

```bash
./bin/composesafe upgrade app=2.1 -f compose.yml                  # dry run: shows the risks, the exact file change, what is stopped
./bin/composesafe upgrade app=2.1 -f compose.yml --execute        # asks you to confirm
./bin/composesafe rollback -f compose.yml --execute               # go back to the latest snapshot
```

`upgrade` assesses like `plan`, takes a verified snapshot, then changes **only the image value** in your compose file
(comments and formatting stay), pulls, starts the new version with `--no-deps`, waits for health and runs your smoke checks.
If anything fails it rolls back by itself: if the new version never started, the files and the old image are put back; if it ran,
the data is restored from the snapshot into **new** volumes and your compose file is pointed at them. The volume that holds the
failed state is never touched. Data written after the snapshot is lost, and the report says from when. HIGH/CRITICAL risks must be
accepted interactively, or with `--yes --allow-risk=<id>`. Ctrl-C does not roll back by itself: run `rollback --run <id> --execute`.

`scan` changes nothing. It lists each service's image, mounts and healthcheck, and shows which services
hold data (official Postgres, Postgres derivatives, other databases, data directories) and why.

## Configuration

Optional `composesafe.yaml` next to the compose file; see the field table in DESIGN section 8.1.
Precedence: flags > `COMPOSESAFE_*` environment > file > defaults. Secrets (API keys) are never read
from the file, only from environment variables.

A **Postgres major-version upgrade** (e.g. `upgrade db=18-alpine`) never upgrades the data in place. After the snapshot the services that
write to the database are stopped, a final `pg_dumpall` is taken, an empty cluster of the new version is started on a **new** volume
(`<volume>_pg18`, or `<dir>_pg18` for a bind mount), the dump is restored, statistics are refreshed (ANALYZE) and the tables are
compared; then the writers come back. For Postgres 18 the mount point moves to `/var/lib/postgresql` as the new image expects.
Your original volume is never written to by the new version, so a failed migration simply switches back to it: nothing is lost,
and the migrated copy is kept for you to inspect. After a successful upgrade the old volume stays until you remove it.

```bash
./bin/composesafe rehearse db=18-alpine -f compose.yml     # runs the whole upgrade on a private copy, removes it afterwards
./bin/composesafe rehearse app=2.1 -f compose.yml --keep   # keep the copy to look around
```

`rehearse` builds a copy of the stack under its own project name (own containers and volumes, no published ports, **no route to the outside**
unless `--allow-egress`), copies your data into it (Postgres from the running database by dump, other volumes as files; your volumes are only
read), starts it in the current version, and then runs the real `upgrade` on it: snapshot, change, verify, rollback on failure. Smoke checks run
from a helper container inside the copy's network (`smoke_helper_image`, default `curlimages/curl`). Production is never stopped or changed.
Stacks that cannot be isolated (host networking, the Docker socket) are refused. A pass is evidence, not a promise: data volume, load and the
outside world differ. Use `--rehearse-env extra.yml` to give the copy rehearsal-only settings.

```bash
./bin/composesafe history -f compose.yml                  # snapshots, upgrades and rehearsals, newest first
./bin/composesafe report latest -f compose.yml            # the Markdown report of a run (an id prefix is enough)
./bin/composesafe cleanup old-volumes -f compose.yml      # list volumes that finished upgrades left behind
./bin/composesafe cleanup old-volumes -f compose.yml --execute   # delete them, one by one, after you type each name
./bin/composesafe cleanup 20261106-1010 -f compose.yml --execute # delete one run (snapshot, dumps, archives, image pins)
./bin/composesafe cleanup rehearsals --execute            # remove rehearsal copies kept with --keep
```

`cleanup` is the only command that deletes your data. It lists first; with `--execute` every item is deleted only after you type its
name, there is no `--yes`, and it needs a terminal. A volume is offered only when a finished upgrade or rollback left it behind and no container
and none of your current compose files use it.
