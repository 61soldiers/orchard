# Agent guide

Orchard is a self-hosted Go server that signs in to an Apple Music subscription and exposes a small
HTTP API (catalog, streaming, downloads, library). [README.md](README.md) is the user-facing guide;
[docs/reference.md](docs/reference.md) is the API and configuration reference;
[CLAUDE.md](CLAUDE.md) is the internals map and the record of *why* the code is shaped as it is. Read
the relevant section of `CLAUDE.md` before changing a package, and update it when you change one.

## Build, test, run

```shell
go build ./...                      # Go 1.26+; pure Go, no cgo, no ffmpeg
go vet ./... && go test ./...       # offline: the tests never reach Apple
export ORCHARD_API_KEY=$(openssl rand -base64 48 | tr -d '=+/')
go run ./cmd/orchard --addr 127.0.0.1:8080 --data-dir ./data
```

Cross-compile with `GOOS`/`GOARCH` and `CGO_ENABLED=0` (linux, darwin, windows all build). The Docker
image (`docker compose build`) is the self-hosting route, not a requirement for development.

Real Apple behaviour (sign-in, streaming, downloads) can only be checked against a live subscription;
the `orchard-live-test` skill under `.claude/skills/` describes how, without replacing a running
setup's API key.

## Layout

```
cmd/orchard        wiring (config → store → wrapper → catalog → API)
cmd/guestbuild     builds the QEMU guest (kernel, base initramfs, data disk)
internal/api       HTTP handlers; never talk to the daemon directly
internal/apple     the sign-in state machine on top of the daemon
internal/wrapper   provisioning, supervision, and how the daemon is hosted (namespaces, proot, QEMU VM)
internal/guest     the VM guest's pieces (cpio writer, init script, base image)
internal/catalog   Apple's catalog and library APIs
internal/stream    decrypting streams; internal/download — jobs, remux (internal/m4a), tagging
internal/store     SQLite (modernc.org/sqlite)
```

## Rules that bite

- **Never log or persist** the API key, Apple credentials, the Music-Token, or a stream URL carrying
  `?api_key=`. Credentials are only ever passed for one login run, then the daemon restarts without them.
- The daemon's four ports are unauthenticated and must stay on loopback.
- A response shape is a contract with the client (the Elbert Apple Music plugin's `src/orchard/client.ts`,
  in another repo). Changing one means changing that file too.
- Don't reintroduce ffmpeg or any other external program; tagging and remuxing are in-process.
- The daemon launcher must be exec'd as `./wrapper` from its own directory; the guest's CPU model must be
  `Nehalem`. Both look like arbitrary choices and both are load-bearing (see `CLAUDE.md`).
- Don't run broad `pkill`/`pgrep -f` patterns on a machine that also runs a container: its processes
  match.

## Commits and releases

Commit messages are **Conventional Commits**: they decide the next version.
`feat:` → minor, `fix:` → patch, a `BREAKING CHANGE:` footer → major; `chore:`, `docs:`, `ci:`,
`test:` and `refactor:` release nothing. `main` is the release branch and is changed through pull
requests. A push to it runs the tests, tags the version, publishes the GitHub release with notes, builds
every platform and attaches the archives and `checksums.txt` (see `.github/workflows/release.yml`). Do
not commit or push unless asked.
