# ComposeSafe

[中文](README.zh-CN.md)

A safety net for upgrading Docker Compose stacks: **assess the risk, take a verified snapshot, upgrade, verify, and roll back if anything breaks** — and rehearse the whole thing on a private copy first.
Commands that change anything default to a dry run. The command is `composesafe`; `csafe` is a short alias.

> Status: **pre-release (v0.x)**. Everything below works and is tested against real Docker, except what is listed under [Limitations](#limitations).
> Design and decisions: [docs/PRD.md](docs/PRD.md), [docs/DESIGN.md](docs/DESIGN.md), [docs/ACCEPTANCE.md](docs/ACCEPTANCE.md).

## What it protects you from

| Without it | With it |
|---|---|
| A new image migrates your database forward, then fails; the old image can no longer read it | The data is restored from a verified snapshot into a **new** volume; the failed state is kept untouched |
| A floating tag (`latest`) moved, so "going back" pulls the new image again | The old image is pinned locally and restarted with `pull_policy: never` |
| A Postgres major upgrade needs a dump/restore and a changed mount point (PG 18) | Done for you into a new volume; your original volume is never written to |
| You find out the upgrade breaks only in production | `rehearse` runs the real upgrade on a copy of the stack and its data, with no way out to the network |
| A "backup" that cannot be restored | Checksums, archive integrity, the dump's end marker, and a test restore into a throwaway server |

## Install

Download the archive for your platform from the releases page (`composesafe_<version>_<os>_<arch>.tar.gz`, with `SHA256SUMS`), or build:

```bash
make build        # bin/composesafe and a bin/csafe symlink
make install      # into $GOBIN
```

Needs Go 1.24+ to build; to run: **Docker Engine 27+ and Docker Compose 2.24+ on the same machine as the project** (a remote `DOCKER_HOST` is refused: backups mount host paths).

## Five-minute tour

```bash
cd your-project            # where compose.yaml lives

composesafe scan                                     # services, images, volumes, which hold data
composesafe plan db                                  # newer tags for db's image
composesafe plan db=17.2                             # assess: release notes, risks, disk; writes report.md
composesafe rehearse db=17.2                         # run the whole upgrade on a private copy; removes it afterwards
composesafe upgrade db=17.2                          # dry run: the exact file change, what stops, risks to accept
composesafe upgrade db=17.2 --execute                # snapshot -> change -> verify -> roll back by itself on failure
composesafe rollback --execute                       # go back to the latest snapshot by hand
composesafe history                                  # snapshots, upgrades, rehearsals
composesafe report latest                            # the Markdown report of a run
composesafe cleanup old-volumes                      # list volumes finished upgrades left behind (then --execute)
```

Try it without risk on the bundled stack: `composesafe scan -f testdata/stacks/toyapp/compose.yml -p toy`.

## Commands

| Command | Changes anything? | What it does |
|---|---|---|
| `scan` | no | Lists services, images, volumes, and which services hold data |
| `plan <svc>[=tag]` | no | Candidate tags; with a target: release notes, risk rules, disk check, `report.md` |
| `snapshot <svc>...` | with `--execute` | Config backup, image pins, `pg_dumpall`, cold volume archives, verification, test restore. Stopped services always come back |
| `upgrade <svc>=<tag>` | with `--execute` | Assess, snapshot, change **only the image value** (comments and formatting stay), pull, `up --no-deps`, health + smoke checks, automatic rollback |
| `rollback [--run id]` | with `--execute` | Restore files and old image; restore data into new volumes if the new version ran |
| `rehearse <svc>=<tag>` | only its own copy | Real upgrade on a private copy of stack and data; production is only read |
| `history` / `report <id>` | no | List runs / print a run's report (an id prefix or `latest` is enough) |
| `cleanup <id\|old-volumes\|rehearsals>` | **deletes**, with `--execute` | The only command that deletes your data; every item needs its name typed; no `--yes` |

Global flags: `-f/--file`, `-p/--project-name`, `-c/--config`, `--execute`, `--yes`, `--json`, `--lang en|zh`, `--force-unlock`.
`--yes` skips the prompt but never overrides a BLOCK; HIGH/CRITICAL risks need `--yes --allow-risk=<id>` (or an interactive yes).

**Exit codes:** `0` ok · `1` error (including a failed rehearsal) · `2` blocked by a risk rule · `3` upgrade failed and was rolled back · `4` upgrade failed **and** the rollback failed (a manual recovery guide is printed and written to the report).

## How it keeps data safe

- Docker is only driven through the `docker` CLI; nothing is deleted except by `cleanup` (and a rehearsal's own copy). A static check in CI fails the build if destructive calls appear anywhere else.
- A snapshot must **verify** before an upgrade starts. A failed snapshot still restarts the services it stopped.
- Rollback never overwrites or deletes: data goes into a **new** volume/directory, and your compose file is pointed at it. Ctrl-C does not roll back by itself (the state is uncertain); `rollback --run <id>` picks up from the snapshot directory alone.
- If you edit the compose file after an upgrade, `rollback` shows a line diff and asks before overwriting.
- Secrets: `.env` values and `*PASSWORD/SECRET/TOKEN/KEY*` variables are masked in logs and reports. The state directory (`~/.local/state/composesafe/<project>/<run-id>/`, mode 0600) holds dumps and config copies — treat it like the secrets it contains.

### Postgres major versions

`upgrade db=18-alpine` never upgrades the data in place. The writers of the database are stopped, a final `pg_dumpall` is taken, an empty cluster of the new version is started on a **new** volume (`<volume>_pg18`, or `<dir>_pg18` for a bind mount), the dump is restored, statistics are refreshed (`ANALYZE`), table counts are compared, and the writers return. For Postgres 18 the mount point moves to `/var/lib/postgresql` as the new image expects. A failed migration simply switches back to the untouched original.

### Rehearsal

`rehearse` builds a copy of the project under its own name (own containers and volumes, no published ports, **no route to the outside** unless `--allow-egress`), copies the data (Postgres from the running database by dump; other volumes as files, read-only from the source), starts it in the current version, and then runs the real `upgrade` on it. Smoke checks run from a helper container inside the copy's network. Stacks that cannot be isolated (host networking, the Docker socket, read-write single-file mounts) are refused. A pass is evidence, not a promise: data volume, load and the outside world differ.

## Configuration (`composesafe.yaml`, optional)

```yaml
version: 1
lang: en                       # en | zh  (or --lang / COMPOSESAFE_LANG)
state_dir: ~/.local/state/composesafe
retention: {snapshots: 5}      # `history` only hints; nothing is deleted automatically
services:
  db:
    snapshot: {mode: logical+volume, quiesce: [app, worker], restore_check: true}
  app:
    health_timeout: 120s
    smoke:
      - {name: http-ok, type: http, url: 'http://{{addr "app" 8080}}/healthz', expect_status: 200, retries: 10, interval: 3s}
      - {name: port-open, type: tcp, addr: '{{addr "app" 8080}}'}
```

Smoke types: `http`, `tcp`, `exec` (inside the container) and `command` (on the host, no shell). `{{addr "svc" port}}` resolves to the published host port in production and to `svc:port` inside a rehearsal. Unknown keys are rejected; there are no secret fields — API keys come from environment variables. Environment overrides: `COMPOSESAFE_STATE_DIR`, `COMPOSESAFE_LANG`, `GITHUB_TOKEN` (raises GitHub's rate limit).

## Support matrix

| Data | Snapshot | Upgrade | Rehearsal |
|---|---|---|---|
| Official `postgres` image, named volume or bind dir | dump + cold archive + test restore | same major: yes; major: dump/restore into a new volume | yes |
| `postgis`, `timescaledb`, `bitnami/postgresql`, `pgvector` | yes | same major only; a major bump is **blocked** | same major only |
| Other named volumes / bind dirs (MySQL, Redis, files…) | cold archive (the service is stopped) | image change, data rollback from archive | files copied hot (may be inconsistent) |
| Anonymous volumes, tmpfs, NFS/remote volume drivers | skipped with a warning | — | — |

## Limitations

- **No LLM yet.** Release notes are assessed by deterministic rules (keywords, version arithmetic, a Postgres EOL table). `llm:` settings are accepted and ignored (a note is printed); LLM summaries are planned and will never be required.
- Tested end to end on Docker Desktop (macOS) with Postgres 16 → 18; Postgres 14 → 16 and 17 → 18 have not been run against real images. CI runs the integration suite on Linux.
- Postgres: a service that sets its own `PGDATA`, mounts a sub-directory of `/var/lib/postgresql/data`, or runs as a non-root `user:` is not handled specially.
- A compose file whose image comes from an anchor, `extends`, `include` or a variable that is not in `.env` cannot be edited automatically: you get the reason before anything is touched.
- Rehearsal copies non-Postgres data from running volumes; it may not be consistent. `command` smoke checks are skipped in a rehearsal.
- Only a local Docker daemon is supported.

## Development

```bash
make check         # vet, unit tests, gofmt, the "never delete" guard
make lint          # staticcheck (go install honnef.co/go/tools/cmd/staticcheck@2025.1.1)
make integration   # real Docker: 20 scenarios, ~15 minutes; CS_TEST_HELPER=alpine:3.21 to pick the helper image
make dist          # cross-built archives + SHA256SUMS in dist/
```
