# Changelog

## v0.1.0

First public release.

- `scan` / `plan`: list services, images and volumes (and which hold data); assess a target tag with release notes, risk rules and a disk check, written to `report.md`.
- `snapshot`: config backup, image pins, `pg_dumpall`, cold volume archives, checksum and archive verification, and a test restore into a throwaway server.
- `upgrade <svc>=<tag>`: dry run by default; with `--execute` it snapshots, changes only the image value (comments and formatting are kept), pulls, restarts, runs health and smoke checks, and rolls back automatically on failure.
- `rollback`: restores files and the old image; restores data into **new** volumes if the new version already ran. Your original volumes are never written to.
- `rehearse`: runs the real upgrade on a private copy of the stack and its data, with no network access, then removes it.
- Postgres major upgrades (including the PG 18 mount-point change) handled via dump/restore into a new volume.
- `history`, `report`, `cleanup old-volumes`.
- Binaries for Linux and macOS (amd64, arm64) with `SHA256SUMS`, a one-line `install.sh`, and a Homebrew formula.

Known limitations are listed in the README.
