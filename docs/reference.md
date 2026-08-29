# Orchard — technical reference

Everything a normal user needs is in the [README](../README.md). This document covers the
internals: how the Apple Music component works, what privileges it needs, configuration, the HTTP
API, and the security model.

## Status

**Phase 5.** Provisioning, supervision, Apple sign-in (including 2FA), catalog browsing,
on-the-fly decrypted streaming, stored downloads, and hardening against Apple's own hiccups
all work end to end.

| Phase | Deliverable                                                                                   |
| ----- | --------------------------------------------------------------------------------------------- |
| 1 ✅   | Server skeleton: chi, API-key auth, SQLite schema and migrations, config, container image     |
| 2 ✅   | Daemon provisioning and supervision, the `/v1/apple/*` setup state machine with 2FA, setup.sh |
| 3 ✅   | Catalog endpoints: search, album, artist, playlist, song, lyrics, storefront                  |
| 4 ✅   | Download jobs: queue, stored library, SSE progress, file serving, track index                 |
| 5 ✅   | Hardening: rate limits, retries and backoff on transient Apple failures, richer library queries |

## How it works

Apple Music audio is DRM-protected. Decrypting it needs
[`wrapper`](https://github.com/WorldObservationLog/wrapper), a daemon that runs Apple's Android
music libraries in a sandbox and holds a real signed-in session. Orchard installs and supervises
that daemon and puts a REST API in front of it, together with the download pipeline from
[`apple-music-downloader`](https://github.com/zhaarey/apple-music-downloader).

The daemon is Linux-only and architecture-specific, which is where the hardware requirements come
from. Orchard fetches the release matching the host CPU on first start and verifies it against the
SHA-256 digest GitHub publishes for the asset.

### Sign-in flow

`POST /v1/apple/login` starts the daemon with credentials and blocks until Apple either accepts
them or asks for a code. The daemon polls `<base-dir>/2fa.txt` for **60 seconds only**, so a late
`POST /v1/apple/2fa` returns `409 not_awaiting_2fa`.

Once the session is written, Orchard restarts the daemon **without credentials**. That drops the
password from the process argv and proves the persisted session works unattended, which is what
every later start relies on.

A session is only considered valid when both `mpl_db/kvs.sqlitedb` and a non-empty `STOREFRONT_ID`
exist. The database alone is not proof: the daemon creates it while starting up, before
authentication is attempted, so a rejected login leaves one behind.

## Container privileges

The daemon sandboxes itself: `unshare(CLONE_NEWUSER|NEWNS|NEWPID)`, a bind mount of
`/dev/urandom`, a `chroot` into its rootfs, and a fresh procfs mount. Two Docker defaults block
that, so `compose.yaml` relaxes both:

| Option                   | Why it is needed                                                                                                                                                               |
| ------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `seccomp=unconfined`     | The default profile only permits `unshare` when the process holds `CAP_SYS_ADMIN`, which this container does not.                                                              |
| `systempaths=unconfined` | Docker masks `/proc/bus`, `/proc/fs`, `/proc/irq` and `/proc/sys` with read-only submounts. The kernel then rejects the daemon's procfs mount as "too revealing" with `EPERM`. |

This is much weaker than the `--privileged` upstream documents: the container keeps its default
capability set (`CapEff` is empty) and runs as a non-root user. Orchard additionally launches the
daemon in its own user namespace with `Pdeathsig`, so it cannot outlive the server.

On Ubuntu 24.04+, AppArmor also blocks unprivileged user namespaces. Either add
`- apparmor=unconfined` to `security_opt`, or set
`kernel.apparmor_restrict_unprivileged_userns=0`.

## Running without Docker

No container privileges to relax, because your user already has what it needs:

```shell
export ORCHARD_API_KEY=$(openssl rand -base64 48 | tr -d '=+/')
go run ./cmd/orchard --data-dir ./data
```

Needs Go 1.26+, and `ffmpeg` on `PATH` before phase 4. Drive `/v1/apple/*` yourself, or point the
setup script at it:

```shell
ORCHARD_URL=http://127.0.0.1:8080 ./setup.sh
```

## Configuration

The API key is read **only** from the environment, never a flag — `argv` is world-readable through
`/proc/<pid>/cmdline`. Everything else can be either.

| Variable                         | Flag                       | Default        | Meaning                                             |
| -------------------------------- | -------------------------- | -------------- | --------------------------------------------------- |
| `ORCHARD_API_KEY`                | —                          | *(required)*   | Bearer key for every request; minimum 24 characters |
| `ORCHARD_ADDR`                   | `--addr`                   | `:8080`        | Listen address                                      |
| `ORCHARD_DATA_DIR`               | `--data-dir`               | `./data`       | Holds `orchard.db`, `library/`, `wrapper/`          |
| `ORCHARD_LOG_LEVEL`              | `--log-level`              | `info`         | `debug`, `info`, `warn`, `error`                    |
| `ORCHARD_TRUST_PROXY_HEADERS`    | `--trust-proxy-headers`    | `false`        | Honour `X-Forwarded-For` / `X-Real-IP`              |
| `ORCHARD_SHUTDOWN_TIMEOUT`       | `--shutdown-timeout`       | `15s`          | Graceful shutdown budget                            |
| `ORCHARD_WRAPPER_AUTO_PROVISION` | `--wrapper-auto-provision` | `true`         | Fetch the daemon release when missing               |
| `ORCHARD_WRAPPER_TAG`            | `--wrapper-tag`            | *(host arch)*  | Pin a release tag                                   |
| `ORCHARD_WRAPPER_URL`            | `--wrapper-url`            | —              | Fetch the zip from here instead of GitHub           |
| `ORCHARD_WRAPPER_SHA256`         | `--wrapper-sha256`         | —              | Pin the expected archive digest                     |
| `ORCHARD_WRAPPER_HOST`           | `--wrapper-host`           | `127.0.0.1`    | Address the daemon binds; keep it on loopback       |
| `ORCHARD_WRAPPER_DECRYPT_PORT`   | `--wrapper-decrypt-port`   | `10020`        | Sample-decryption service                           |
| `ORCHARD_WRAPPER_M3U8_PORT`      | `--wrapper-m3u8-port`      | `20020`        | Manifest service                                    |
| `ORCHARD_WRAPPER_ACCOUNT_PORT`   | `--wrapper-account-port`   | `30020`        | Account-info service                                |
| `ORCHARD_WRAPPER_KEY_PORT`       | `--wrapper-key-port`       | `40020`        | Key-template service                                |
| `ORCHARD_WRAPPER_DEVICE_INFO`    | `--wrapper-device-info`    | *(upstream's)* | Override the daemon's `-I` device string            |
| `ORCHARD_WRAPPER_PROXY`          | `--wrapper-proxy`          | —              | Proxy for the daemon, e.g. `socks5://host:port`     |
| `ORCHARD_RATE_LIMIT`             | `--rate-limit`             | `20`           | Requests/sec allowed against `/v1`; `0` disables it |
| `ORCHARD_RATE_LIMIT_BURST`       | `--rate-limit-burst`       | `40`           | Burst size for `ORCHARD_RATE_LIMIT`                 |
| `ORCHARD_APPLE_RATE_LIMIT`       | `--apple-rate-limit`       | `5`            | Requests/sec Orchard sends to Apple's catalog API   |
| `ORCHARD_APPLE_RATE_LIMIT_BURST` | `--apple-rate-limit-burst` | `10`           | Burst size for `ORCHARD_APPLE_RATE_LIMIT`           |

## API

Every `/v1` route needs `Authorization: Bearer <key>`. `GET /healthz` does not.

### Session

```text
GET    /healthz              liveness, no auth
GET    /v1/apple/status      session + daemon install state
POST   /v1/apple/login       {appleId, password}
POST   /v1/apple/2fa         {code}
DELETE /v1/apple/session     log out and wipe the stored session
```

`state`: `unconfigured` · `starting` · `awaiting_2fa` · `ready` · `failed`
`wrapper.state`: `absent` · `installing` · `ready` · `failed` · `unsupported`

Read `state` from the body rather than relying on the HTTP status: `login` returns 200 with
`state: awaiting_2fa` when Apple wants a code, and 502 with the failure detail when it does not.

### Catalog

```text
GET /v1/storefront                  -> {storefront}
GET /v1/search?term=&types=&limit=  -> {songs,albums,artists,playlists}
GET /v1/albums/{id}                 -> album with full track list
GET /v1/artists/{id}                -> artist with albums
GET /v1/playlists/{id}              -> playlist with all tracks (paged transparently)
GET /v1/songs/{id}                  -> single song
GET /v1/songs/{id}/lyrics           -> {songId, format:"ttml", ttml, lrc, syncLevel}
```

`types` is a comma-separated subset of `songs,albums,artists,playlists` and defaults to all four.
`limit` defaults to 25 and is clamped to Apple's maximum of 25.

The storefront comes from the signed-in account, so results match that region. Responses are
Orchard's own shapes, not Apple's — the client normalises them so the API stays stable if Apple
reshuffles theirs.

Every item carries a `quality` object derived from Apple's `audioTraits`:

```json
{ "lossless": true, "hiRes": false, "atmos": true, "spatial": true }
```

`artwork.url` is Apple's template containing `{w}` and `{h}`; `artwork.thumbUrl` is the same image
already resolved to 600×600 for clients that just want to render something.

`/v1/songs/{id}/lyrics` converts Apple's TTML to LRC server-side, so a client never has to parse
TTML itself. Apple declares the sync tier directly on the TTML root (`itunes:timing="Word" |
"Line" | "None"`, confirmed against live responses — not guessed at by counting elements), and
`lrc`/`syncLevel` reflect whichever tier the conversion actually landed on:

- `"word"` — the informal "enhanced LRC" shape, one inline `<mm:ss.xxx>` tag per word ahead of the
  line's own `[mm:ss.xxx]` tag: `[00:08.789]<00:08.789>Hello <00:09.100>world`. Validated against a
  structurally accurate synthetic sample; a real word-by-word ("Apple Music Sing") track has not
  turned up through this endpoint in testing — it may be gated to a narrower surface than the
  general catalog lyrics call.
- `"line"` — standard `[00:08.789]full line text` LRC. The common case for anything with
  `hasTimeSyncedLyrics: true`.
- `"none"` — bare text lines, no time tags at all.

`lrc` is empty (with `ttml` still populated) if conversion fails outright — malformed TTML, or a
tier's data turning out unusable after all (falls through to the next tier down before giving up
entirely; see `ttmlToLRC` in [internal/catalog/lyrics.go](internal/catalog/lyrics.go)).

Catalog failures map to `404 not_found`, `503 apple_not_ready` (not signed in),
`502 apple_unauthorized` (session expired — sign in again) and `502 apple_error` with Apple's own
detail. A transient failure — a dropped connection, a 429, a 5xx — is retried internally with
exponential backoff (honouring Apple's `Retry-After` on a 429) before it ever reaches the client;
`502 apple_error` means retries were exhausted, not that none were tried. Outbound calls to Apple's
catalog API are also paced to `ORCHARD_APPLE_RATE_LIMIT` requests/sec, independently of the 8
requests Orchard allows in flight at once.

Orchard's own `/v1` surface is rate limited to `ORCHARD_RATE_LIMIT` requests/sec per key (default
20, burst 40): a client that exceeds it gets `429 rate_limited` with a `Retry-After` header. This
protects the Apple session from a runaway client as much as it protects Orchard itself.

### Personalization

```text
GET /v1/me/recommendations       -> {groups:[{id,title,reason,items:[...]}]}
GET /v1/me/recent/played?limit=  -> {items:[...]}
```

This is the signed-in account's own personalization, not storefront catalog browsing: "Made For
You" and recently played. Confirmed against a live account, `groups[]` does **not** give each
personal mix its own row: Apple bundles several named playlists — `"New Music"`, `"Heavy
Rotation"`, `"Your Essentials"`, `"Get Up!"`, `"Chill"` — into one umbrella group such as
`"Playlists Made for You"`, alongside themed collections (`"Film, TV & Stage"`, `"New Releases for
You"`) and station groups (`"Stations for You"`) whose `items` run to a dozen or more, mixing
playlists, albums and stations freely. A client looking for one mix by name — "the weekly new
music playlist", "the discovery station" — has to search `items[].name` across every group, not
`groups[].title`; `title`/`reason` describe the group as a whole (`reason` is usually absent,
present mainly on the more editorial collections, e.g. `"Get to know the Gen Z trendsetters
thinking about music differently."`).

Both endpoints return `Item`, a minimal, uniform shape, since the list mixes songs, albums,
playlists and stations:

```json
{ "id": "pl.pm-20e9f373919da08024ee3a0b4caa3d84", "type": "playlists", "name": "New Music",
  "curatorName": "Apple Music for <you>", "artwork": { "url": "...", "thumbUrl": "..." } }
```

"Discovery Station" (Apple's personalized radio) shows up the same way, as a `stations`-typed
item inside `recent/played` or a `"Stations for You"` group — never as a playlist.

Fetch the full resource by `id`/`type` through the catalog endpoints above — `GET
/v1/playlists/{id}` for a `playlists` item, `GET /v1/albums/{id}` for `albums`, `GET
/v1/songs/{id}` for `songs`. A `stations` item is display-only: Orchard's pipeline decrypts a
song's HLS asset and has no equivalent for Apple's live radio, so a station can be shown but not
played or downloaded.

### Streaming

```text
GET /v1/songs/{id}/variants          -> {variants:[{codec,groupId,bandwidth,sampleRate,bitDepth}]}
GET /v1/songs/{id}/stream?codec=     -> audio/mp4, decrypted, streamed as it arrives
```

`codec` is `alac` (default), `atmos` or `aac`. The highest-bandwidth rendition for that codec is
chosen.

**The response is a fragmented MP4, not a plain `.m4a`.** A progressive file needs a complete
sample table in its `moov`, which is only known after the whole track has been processed — so it
cannot be produced incrementally. Fragmented MP4 can, which is what makes playback start before
the download finishes. ffmpeg-based players (including libmpv) handle it natively; browsers using
Media Source Extensions do not support ALAC at all. Pipe it through `ffmpeg -c copy` for a
seekable `.m4a`.

How it works: the daemon resolves an HLS manifest, whose audio is one long byte-range asset made
of ~15-second `moof`/`mdat` fragments. Orchard reads that asset as a stream and, for each
fragment, decrypts every sample through the daemon's port 10020 oracle, strips the encryption
boxes and flushes the result immediately. Nothing buffers the whole track. Apple rotates the key
mid-track — the first fragment uses a placeholder the daemon expects under adam id `0` — which the
pipeline handles transparently.

A genuine seek is refused with `416`: re-decrypting from an arbitrary offset isn't supported, so
download the track instead once that lands. `Range: bytes=0-` is the one exception — it's treated
as no Range at all rather than refused, because it means "everything, from the start" (exactly
what a plain `200` already provides) and it's what ffmpeg/libmpv's HTTP protocol sends by default
on *every* open, not just an actual seek; refusing it made this endpoint fail to open in any
native media player.

Manifest resolution round-trips to Apple and can take up to a minute on a cold cache, so resolved
manifests are cached for 5 minutes. Streaming itself runs far faster than real time — a 3:50
lossless track takes a few seconds on a fast connection.

A small set of older, AAC-only catalog items were never re-encoded for FairPlay streaming: the
daemon resolves them not to an HLS master playlist but to a single-file asset
(`streamingaudio.itunes.apple.com/.../mzaf_*.m4p`) that is the full track wrapped in legacy iTunes
DRM (`stsd` = `drms`, `schm` = `itun` — the pre-2009 iTunes Store purchase scheme). The
wrapper/FairPlay pipeline here has nothing to decrypt, so `stream.Variants` returns
`ErrNoFairPlayAsset`.

**Both `/stream` and downloads fall back to a Widevine path** for these (see
[internal/webplayback](../internal/webplayback)): `POST .../wa/webPlayback` with the media-user
token returns a `flavor 28:ctrp256` AAC-256 asset, decrypted with a bundled public Widevine L3 CDM
(`github.com/iyear/gowidevine`) — no daemon involved. It mirrors `apple-music-downloader`'s
`--aac aac-lc` mode.

- `/stream` fetches and decrypts the whole asset in memory first (a few MB, so a few seconds'
  time-to-first-byte), then serves it exactly like the FairPlay stream — a plain `200`, whole
  body, `Accept-Ranges: none`, same `bytes=0-` allowance. It is **not** seekable: there is no
  cache, so honouring a seek would re-fetch and re-decrypt the whole track per probe, and libmpv
  probes hard the moment a stream looks seekable, which stalls playback. Downloaded copies are the
  seekable option. `/v1/songs/{id}/variants` still returns `422 no_fairplay_asset` since that path
  has no selectable renditions.
- `download.Manager` decrypts the same asset to a file; the stored track is recorded as
  `codec: aac` regardless of the codec the job asked for.

### Downloads

```text
POST   /v1/downloads              {type, id, codec, trackIds?} -> 202 job
GET    /v1/downloads              recent jobs
GET    /v1/downloads/{id}         one job with per-track progress
GET    /v1/downloads/{id}/events  SSE progress
DELETE /v1/downloads/{id}         cancel

GET    /v1/tracks/{id}/file       the finished .m4a, supports Range
GET    /v1/library/tracks         downloaded tracks: filter, search, sort and page
GET    /v1/library/albums         downloaded tracks grouped by album
GET    /v1/library/artists        downloaded tracks grouped by artist
GET    /v1/library/stats          track/album/artist counts and total size and duration
```

`type` is `song`, `album`, `artist` or `playlist`; the catalog item is expanded into tracks at
enqueue time. `trackIds` optionally narrows an album or playlist to a subset. `codec` matches the
streaming endpoint.

Job states: `queued` · `running` · `done` · `failed` · `cancelled`. Per-track phases: `queued` ·
`manifest` · `download` · `decrypt` · `remux` · `tag` · `done` · `failed`. A job whose tracks partly
fail still ends `done`, with `error` describing how many failed.

SSE replays the job's current state on connect, so a client joining late is not left waiting for
the next change, then streams updates until a terminal state. It sends `: ping` comments every 20
seconds to stop proxies closing an idle connection.

Tracks are processed one at a time. The daemon's decryption service handles a single connection,
so concurrency there buys nothing.

Files land in `<data>/library/<album>/<track number> - <title>.m4a`. The remux is a stream copy —
audio is never re-encoded — with tags and cover art applied in the same ffmpeg pass. Unlike the
streaming endpoint these are progressive files, so `Range` requests and seeking work.

A `.lrc` sidecar lands next to the `.m4a` at the same base name whenever the track has lyrics —
same best-tier conversion as `/v1/songs/{id}/lyrics` (word, falling back to line, falling back to
plain text). Best-effort like cover art: no attempt when Apple reports no lyrics, and any failure
is silent rather than failing the download.

Jobs left `running` by a crash are marked failed at startup rather than lingering.

SSE routes also accept `?api_key=…`, because the browser `EventSource` API cannot set headers.
Prefer the header everywhere else — query strings leak into proxy logs and browser history.

### Library queries

`GET /v1/library/tracks` defaults to every downloaded track, oldest download first — the shape a
sync client relying on `since` has always seen. Every parameter is optional and composable:

```text
since=<RFC3339>     only tracks downloaded at or after this instant
q=<text>            substring match against title, artist or album (case-insensitive)
artist=, album=     exact match, case-insensitive
albumId=            the catalog album id recorded at download time (see below)
codec=              alac | atmos | aac
sort=               downloadedAt (default) | title | artist | album | duration | size
order=              asc (default) | desc
limit=, offset=     limit defaults to 500 and is capped at 1000
```

`GET /v1/library/albums` and `GET /v1/library/artists` group the same tracks, each row
carrying a track count, total size, and — for albums — the summed duration and the artist
name (or `"Various Artists"` when a compilation's tracks disagree). Albums are grouped by
catalog album id when one was recorded, falling back to the album name for anything downloaded
before that field existed; a compilation's `albumId` is stable even if Apple's own album title
is later edited. `GET /v1/library/stats` rolls the whole library up into one object:
`trackCount`, `albumCount`, `artistCount`, `totalSizeBytes`, `totalDurationMs`.

## Storage and backup

```text
<data>/
  orchard.db              jobs, per-track progress, track index
  library/                <albumId>/<trackId>.m4a
  wrapper/                the provisioned daemon and its rootfs
    .provision.json       installed tag and digest
    rootfs/data/…         the Apple session; preserved across daemon upgrades
```

```shell
docker run --rm -v orchard_orchard-data:/data -v "$PWD:/backup" \
  debian:bookworm-slim tar czf /backup/orchard-backup.tar.gz -C /data .
```

To use a host directory instead of the named volume, swap the volume line in `compose.yaml` for
`- ./data:/data` and run `sudo chown -R 10001:999 ./data` first — the container is not root and
cannot write to a directory owned by you.

## Security

- **Serve over TLS only.** The compose file binds to `127.0.0.1` so it cannot be reached directly.
- **Never expose the daemon's ports** (10020, 20020, 30020, 40020). They are unauthenticated
  access to a live Apple Music session and stay on `127.0.0.1`.
- The Apple password is never written to disk or the database, and never logged. It is, however,
  **briefly visible in `/proc/<pid>/cmdline`**: upstream accepts credentials only through its `-L`
  flag, so argv cannot be avoided. Orchard limits the window by restarting the daemon without
  credentials as soon as the session is saved; every later start uses none at all.
- The daemon prints a `Music-Token` prefix at startup; that line is redacted before it is logged.
  The request logger records method, path and status only — never headers or query strings.
- Downloads are verified against the SHA-256 digest GitHub publishes. That covers corruption and
  tampering in transit, not a malicious upstream — pin `ORCHARD_WRAPPER_SHA256` to freeze the
  install to a digest you have reviewed. Extraction rejects absolute paths, `..` traversal and
  symlinks, and caps the tree at 512 MiB.
- Only set `ORCHARD_TRUST_PROXY_HEADERS=true` behind a proxy you control; otherwise clients can
  spoof their address.
- The `chroot` and user namespace are a compatibility shim, not a security boundary: the daemon
  has full network access and runs as your user. Do not run Orchard as root.

## Known fragility

The daemon hooks a hardcoded address (`libCoreLSKD+0x1d5709`) inside Apple's library, so it breaks
whenever Apple ships a new Music APK. If things start failing after an upstream release, pin a
known-good build with `ORCHARD_WRAPPER_TAG` and `ORCHARD_WRAPPER_SHA256`, and treat upgrades as a
manual, tested step.
