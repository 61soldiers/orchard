# Orchard

Self-hosted server that signs in to a real Apple Music subscription and puts a small, stable
REST API in front of it — catalog browsing, on-the-fly decrypted streaming, and a download
pipeline that lands tagged `.m4a` files on disk. It exists so a client (a music player, a script,
a home-server integration) never has to speak Apple's private APIs, hold the Apple session
itself, or know anything about DRM.

Orchard does not decrypt anything itself. It provisions and supervises
[`wrapper`](https://github.com/WorldObservationLog/wrapper) — a daemon that runs Apple's own
Android Music libraries in a sandbox and holds the signed-in session — and borrows the download
orchestration approach of [`apple-music-downloader`](https://github.com/zhaarey/apple-music-downloader)
(same author who built the reference implementation `wrapper`'s protocol is meant for). Orchard's
own contribution is the daemon lifecycle (auto-provisioning, supervision, first-run 2FA), the HTTP
API, the SQLite-backed job/library store, and hardening (rate limits, retries).

**Known consumer**: [elbert](../elbert) (a Flutter music player) has a compile-time-optional Apple
Music tab that talks to this API — see `elbert/CLAUDE.md`'s "Apple Music (Orchard)" section and
`elbert/lib/services/orchard_service.dart` for the client-side shape every response below maps to.
When changing a response shape here, that file needs the matching update.

- Not a git repository (no `.git`) — there is no commit history to consult; this file and
  [docs/reference.md](docs/reference.md) are the record of *why*, not `git log`.
- Single-tenant: one API key, one Apple session, no user accounts. Every DB row is unscoped.
- The daemon is Linux `amd64`/`arm64` only — no build for anything else. The *host* can be Linux,
  macOS or Windows: on macOS and Windows the daemon runs unchanged inside Docker Desktop's Linux
  VM (WSL2 on Windows, LinuxKit on macOS — both ship with unprivileged user namespaces on).
  `setup.sh` covers Linux and macOS (it branches on `uname -s` — skips the host `/proc` userns
  check on Darwin, avoids `sed -i`); `setup.ps1` is the Windows/PowerShell port. Anything outside
  a Linux container (Windows containers, a non-WSL2 Docker backend) cannot work — the daemon needs
  `unshare(CLONE_NEWUSER|NEWNS|NEWPID)`. macOS is newer and less battle-tested than Linux.

---

## Status

**Phase 5 (hardening) is done.** All five phases are complete; nothing is scaffolded or partial.

| Phase | Deliverable                                                                                   |
| ----- | --------------------------------------------------------------------------------------------- |
| 1 ✅   | Server skeleton: chi, API-key auth, SQLite schema and migrations, config, container image     |
| 2 ✅   | Daemon provisioning and supervision, the `/v1/apple/*` setup state machine with 2FA, setup.sh |
| 3 ✅   | Catalog endpoints: search, album, artist, playlist, song, lyrics, storefront                  |
| 4 ✅   | Download jobs: queue, stored library, SSE progress, file serving, track index                 |
| 5 ✅   | Hardening: rate limits, retries and backoff on transient Apple failures, richer library queries |

Read [docs/reference.md](docs/reference.md) for the full HTTP API, config table and security
model — this file is the internals map (why the code is shaped this way, what would break if you
changed it), not a duplicate of that reference.

---

## Commands

```shell
go build ./...                          # compile everything
go vet ./...                            # static checks — keep clean
gofmt -l .                               # should print nothing; gofmt -w . to fix
go run ./cmd/orchard --data-dir ./data   # run directly; needs ORCHARD_API_KEY (24+ chars) in env

docker compose build                    # rebuild the image from local source
docker compose up -d                    # (re)create the container; the named volume (Apple
                                         # session + library + db) survives a rebuild/recreate
docker compose logs -f orchard          # tail structured JSON logs (set ORCHARD_LOG_LEVEL=debug
                                         # in .env to see the daemon's own stderr, tagged "wrapper")
./setup.sh                              # interactive: start the stack, wait for the daemon to
                                         # install, walk through Apple sign-in + 2FA. Idempotent.
                                         # POSIX-portable — runs on Linux and macOS (branches on
                                         # `uname -s`, no `sed -i`, no bash 4+ features).
# setup.ps1 is the equivalent Windows/PowerShell port of setup.sh — keep all three flows in sync
# when the setup steps or the /v1/apple/* state names change.
```

There are no `_test.go` files anywhere in the tree. Verify a change by building, running (`go run`
or the container) against a real, logged-in Apple Music account, and hitting the relevant
endpoint with `curl` — the `orchard-live-test` skill
([.claude/skills/orchard-live-test/SKILL.md](.claude/skills/orchard-live-test/SKILL.md))
automates exactly that rebuild → restart → verify cycle against the real account already set up
on this machine.

Module path is bare `orchard` (see `go.mod`); internal packages import as `orchard/internal/...`.
Go 1.26+.

---

## Architecture

```
cmd/orchard/main.go   wiring: config → store → wrapper provisioner/manager → catalog client →
                       stream client → download manager → HTTP server → graceful shutdown
internal/
  config/              flag+env parsing (config.Config), the only place ORCHARD_* is read
  wrapper/             provisions (downloads/verifies the daemon release) and supervises
                       (starts/stops/parses stderr from) the wrapper process
  apple/               owns session lifecycle on top of wrapper: login/2FA state machine,
                       account token cache, exposes CatalogTokens() to the catalog client
  catalog/             Apple Music catalog HTTP client — search/album/artist/playlist/song/
                       lyrics/recommendations/recently-played — with its own rate limiter
                       and retry/backoff, independent of everything else
  playactivity/        the one *write* to Apple: reports a play so it lands in the account's
                       Recently Played; wire format read off Apple's own musickit.js
  stream/              resolves a track's HLS manifest and decrypts it fragment-by-fragment
                       through the daemon's decrypt oracle; also vendors the cbcs sample
                       decryption logic (stream/decrypt.go)
  download/            turns a catalog request into files on disk: one job, tracks processed
                       serially, ffmpeg remux+tag, writes to store.Track on completion
  store/               SQLite (modernc.org/sqlite, no cgo): jobs, per-track progress, the
                       downloaded-track index, migrations
  ratelimit/           tiny self-contained token bucket (no dependency), used both as inbound
                       HTTP middleware and to pace outbound calls to Apple
  retry/               generic backoff-with-jitter helper, used by catalog and stream
  api/                 chi router, handlers, auth middleware, error mapping
```

Dependency direction is strict and one-way: `api` →
`download`/`catalog`/`stream`/`playactivity`/`apple`/`store` → `wrapper`/`ratelimit`/`retry`. Nothing in `internal/` imports `api`, and `catalog`/`stream` don't
import each other or `download` — `download.Manager` is the only package that holds references to
both, because it's the only thing that needs a track's metadata *and* its decrypted bytes together.

### Request flow for anything Apple-backed

1. `api` handler calls into `catalog`/`stream`/`download`.
2. That package needs tokens → calls `apple.Manager.CatalogTokens()` (a `catalog.TokenSource`
   function value, not an interface — see `catalog.New`'s signature).
3. `apple.Manager.Account()` returns cached tokens (5 min TTL) or fetches fresh ones from the
   daemon's account-info service (`127.0.0.1:<AccountPort>`), erroring `apple.ErrNotReady` if the
   session state isn't `ready`.
4. The actual Apple HTTP call goes through `catalog.Client.get()` (or `stream.Client.fetch()`),
   both of which: wait on their own rate limiter, retry transient failures (network error, 429,
   5xx) with backoff, then decode.

A handler never talks to `wrapper` or the daemon's raw ports directly — always through `apple` (for
session/tokens) or `stream` (for the manifest/decrypt ports).

---

## The Apple session (`apple` + `wrapper`)

Two packages, two concerns, one lifecycle:

- **`wrapper.Provisioner`** ([internal/wrapper/provision.go](internal/wrapper/provision.go)) —
  downloads and verifies the daemon release for the host arch (`amd64`→`wrapper.x86_64.latest`,
  `arm64`→`wrapper.arm64.latest`; nothing else is supported). Idempotent: `Ensure()` checks a
  `.provision.json` manifest and only re-downloads if the tag/digest pin doesn't match. Carries an
  existing Apple session (`rootfs/data/`) across a daemon upgrade since that's the only
  non-reproducible part of the tree. Extraction rejects absolute paths, `..`, and symlinks, and
  caps the tree at 512 MiB.
- **`wrapper.Supervisor`** ([internal/wrapper/supervisor.go](internal/wrapper/supervisor.go)) —
  starts the daemon in its own user+mount+PID namespace (`Pdeathsig: SIGKILL` so it can never
  outlive Orchard), parses its stderr line-by-line into typed `Event`s (`needs_2fa`,
  `code_accepted`, `login_failed`, `listening`, …), and redacts the `Music-Token` line before
  logging. `WaitListening` polls all four TCP ports rather than trusting a log line, since the
  daemon can print "listening" before the socket is actually accept()-ing.
- **`apple.Manager`** ([internal/apple/apple.go](internal/apple/apple.go)) — the state machine:
  `unconfigured → starting → awaiting_2fa → ready` (or `→ failed` from most states). `Login()`
  blocks until one of those is reached; `Submit2FA()` the same. `LoggedIn()` requires **both**
  `mpl_db/kvs.sqlitedb` and a non-empty `STOREFRONT_ID` to exist — the DB alone isn't proof, since
  the daemon creates it on startup before authenticating, so a rejected login can leave one
  behind. After a successful login, `finishLogin()` restarts the daemon **without** credentials —
  this is the whole mitigation for credentials being visible on argv (`/proc/<pid>/cmdline`) during
  `-L`.
- **`apple/account.go`** — `Account()` caches the daemon's dev/media-user tokens for 5 minutes and
  adapts them to `catalog.TokenSource` via `CatalogTokens()`. Never log either token.

If you add a new Apple-facing package, it needs a `TokenSource`-shaped dependency on
`apple.Manager.CatalogTokens`, not a direct reference to `apple.Manager` — keeps `catalog` (and
any future package) independently testable and free of the session state machine.

---

## Catalog (`internal/catalog`)

`Client` ([internal/catalog/client.go](internal/catalog/client.go)) talks to
`amp-api.music.apple.com` as the signed-in account. Surface: `Storefront`, `Search`, `Album`,
`Artist`, `Playlist` (metadata + first page of tracks only — see pagination below), `Song`,
`Lyrics`, `Charts`, `Groupings`, `Recommendations`, `RecentlyPlayed`, `StationTracks`, and the
`Library*` methods ([internal/catalog/library.go](internal/catalog/library.go)) behind
`/v1/me/library/*`.

**Playlist tracks paginate; the download pipeline doesn't get to see that.** `Playlist(ctx, id)`
fetches metadata plus only the first page of `relationships.tracks` (however big Apple's own
default page is) and sets `TracksNextCursor` to Apple's own `next` link when there's more —
`PlaylistTracks(ctx, id, cursor)` fetches the next page, `cursor` being that value echoed back
verbatim. This is what `handlePlaylist`/`handlePlaylistTracks` expose over HTTP, so a client opens
a long playlist for the cost of one small request instead of Apple's tracks relationship walked to
the end. `download.Manager` still needs every track to queue a `type: playlist` job, so it calls
`PlaylistFull(ctx, id)` instead — the old walk-until-exhausted behavior, kept under its own name
rather than the default, so a future caller of `Playlist` doesn't silently get the download
pipeline's full-fetch cost. A cursor is validated (`validCursor` in client.go: must be a relative
`/v1/...` path, never an absolute URL) before it's used as a request path — the request that
follows one carries the Apple session's bearer/media-user tokens, so an unchecked cursor would be
an exfiltration vector, not just a correctness bug. `Library*` reuses the identical cursor
contract (see below) via the same `validCursor`.

`Pins` ([internal/catalog/pins.go](internal/catalog/pins.go)) reads `/v1/me/library/pins` — what
Apple's apps call Pins, the items a user pins to the top of their Library. **Not a MusicKit
endpoint and not documented anywhere**: it was found by probing a live account, and the only
resource type actually observed on one was `library-playlists`, so the decoding is deliberately
type-agnostic (one `rawPinAttrs` covering every pinnable type, absent fields simply staying empty)
rather than modelled on the one case we can see. It resolves to catalog ids the same way the
`Library*` methods do; keep the `Pin` shape in sync with elbert's `OrchardPin`.

`Library*` reads the account's *own* added music and playlists (`/v1/me/library/...`, not
storefront-scoped). Apple's library ids (`i.*`/`l.*`/`p.*`) are useless to the stream/download
pipeline, so every item is resolved to its catalog resource via `include=catalog` (fallback:
`playParams.catalogId`) before it leaves — the ids handed to a client are catalog ids.
`LibraryPlaylists`, `LibrarySongs`, `LibraryAlbums`, `LibraryArtists` and `LibraryPlaylistTracks`
each return exactly **one page** (100 items) plus a `next`/`nextCursor`, rather than walking to
the end internally — mirrors the `Playlist`/`PlaylistTracks` split above, and for the same reason:
opening the Apple Music library tab or a big personal playlist shouldn't cost one request that
resolves the whole thing. `LibraryPlaylist(ctx, id)` (singular) is metadata + first tracks page,
same shape as catalog `Playlist`; its `TrackCount` is therefore only the page-so-far count until
`TracksNextCursor` goes empty (documented on the field — don't read it as a true total before
that). `handleLibrary*Me` in [internal/api/library_me.go](internal/api/library_me.go); keep the
`LibraryPlaylist` shape in sync with elbert's `orchard_service.dart`.

`Artist` requests Apple's discography `views` (top-songs, singles, similar-artists, …) and
`extend=artistBio,bornOrFormed,origin` — search still returns only id/name/artwork.
`Album`/`Playlist` request `include[songs]=artists,albums` so nested tracks keep their
`artistId`/`albumId`. `Groupings` reads the undocumented `/v1/editorial/{sf}/groupings` tree
and flattens it to titled rows via `collectEditorialGroups` — amp-api-only and best-effort
(parse failure ⇒ empty, never an error). See [docs/reference.md](docs/reference.md#known-fragility).

Apple returns editorial `notes`/`description`, artist bios and `copyright` as **HTML
fragments**. `notesText` and the `conv*` funcs run every such field through `htmlToText`
([internal/catalog/htmltext.go](internal/catalog/htmltext.go)) — tags stripped, entities
decoded — so the API only ever emits plain text.

**Two independent throttles, don't conflate them**: `maxConcurrent = 8` (a semaphore — how many
requests may be in flight at once) and `Config.RateLimit`/`RateBurst` (a token bucket — how fast
new requests may start), wired from `ORCHARD_APPLE_RATE_LIMIT`/`_BURST` in `main.go`. Both apply to
every `get()` call regardless of which public method it came from.

**Retry lives inside `get()`**, not at the call sites: `isRetryableErr` retries a transport failure,
a 429, or a 5xx (honouring `Retry-After` on 429 via the `retry.Afterer` interface on `*APIError`),
but never `ErrNotFound`/`ErrUnauthorized`/a plain 4xx, and never after `ctx` is done. Tokens are
fetched **once**, outside the retry loop — a `tokens()` failure (usually "session not ready") isn't
a transient HTTP problem and retrying it wastes four attempts for nothing.

**`Recommendations`/`RecentlyPlayed` return `Item`, not `Song`/`Album`/`Playlist`** — confirmed
against a live account, both endpoints mix resource types in one list (a `RecommendationGroup`
commonly bundles several named playlists — `"New Music"`, `"Get Up!"`, `"Chill"` — under one
umbrella title like `"Playlists Made for You"`, and can include albums and `stations` too). Don't
assume one group = one named mix, and don't assume `groups[].title` is ever literally `"New Music
Mix"` — it isn't; see [docs/reference.md](docs/reference.md#personalization) for the shape
observed live. `Item` carries just enough to render a tile (`id`, `type`, `name`,
`artistName`/`curatorName`, `artwork`) — fetch the full resource by id/type through the regular
catalog methods once the caller needs more.

**Apple puts two unrelated things behind the one `stations` type, and only one of them is
playable.** A *personalized* station — Discovery Station, an artist or song station, anything in a
"Stations for You" group — is really a rolling, server-generated queue of ordinary catalog songs,
which `StationTracks` ([internal/catalog/stations.go](internal/catalog/stations.go)) reads via
`POST /v1/me/stations/next-tracks/{id}` (the endpoint Apple's own musickit.js uses, in
`MusicItemLoader.loadStationNextTracks`). Those songs go through the normal stream/download path
like any other track, so nothing new was needed below `catalog`.

**Each call advances the station server-side** — two calls return different songs. There is no
stable track list, which is why the API exposes it as `POST .../next-tracks` and not as a
station-detail GET, and why a client plays a station by fetching a batch and coming back for the
next one as the queue drains (elbert's `AppleMusicStationPlayer` does exactly that).

A *live* station (Apple Música 1 and the other broadcast channels) is the case the old note here
was about: a continuous broadcast with no track list at all. `StationTracks` returns
`ErrStationNotPlayable` for those, mapped to a 422. Nothing here speaks Apple's live-radio
protocol, and building that would still be an entirely new pipeline.

**Library playlists can be written, not just read**
([internal/catalog/library_write.go](internal/catalog/library_write.go)). Create, rename, delete,
append tracks, and replace the whole ordered track list — enough for a client to manage the
account's own playlists. Three things about it are load-bearing:

- **Reorder and remove are the same operation as replace.** Apple has no "move track" or "remove
  one track" on a library playlist, so `SetLibraryPlaylistTracks` sends the complete list in its
  intended order. A caller holding only one page of a paginated playlist would silently truncate
  it — which is why the elbert side walks every page before calling. Clearing a playlist needs an
  explicit `allowEmpty`, so the shape a client bug takes (an empty list) is a 400 rather than a
  wiped playlist.
- **Retries are split by idempotency**, which is why `sendJSON` and `sendIdempotent` are separate
  in [client.go](internal/catalog/client.go) rather than one helper: a retried create makes a
  second playlist and a retried append duplicates tracks, so those two don't retry; `PATCH`/`PUT`/
  `DELETE` do.
- **Ids are catalog ids**, like every other library read here — except one starting with `i.`,
  which is a library-only track (uploaded or matched) and goes back as a `library-songs`
  reference. `trackRefType` is the whole of that rule.

Verified live against the real account (create → add → rename → reorder → remove → delete, all
clean). The one surprise worth knowing: **Apple silently drops a track id it can't resolve** — one
of the three library songs used was no longer in the catalog (`GET /v1/songs/{id}` 404s for it),
and the add still answered `200 {"ok":true}` having added only the other two. A 2xx here does not
mean every id landed.

A successful library write is usually `204 No Content`, and Apple sometimes answers a create with
`201` and an empty body — `doWrite` treats both as success, and `CreateLibraryPlaylist` falls back
to returning what it asked for rather than failing a write that actually happened.

---

## Streaming (`internal/stream`)

Turns a track into a decrypted fragmented MP4. `Variants()` resolves the master playlist and lists
renditions (`alac`/`atmos`/`aac`, picking the highest-bandwidth one per codec in `Pick()`). `Open()`
streams the media playlist's single byte-range asset, decrypting each `moof`/`mdat` fragment
through the daemon's port-10020 oracle as it arrives — nothing is buffered whole-track, which is
what lets playback start before a track finishes.

**Real consumer**: elbert's Apple Music tab plays a track by handing `media_kit` the
`/v1/songs/{id}/stream` URL directly, `?api_key=` and all (see the API section's note on
`presentedKey`'s query-param fallback). This is a live, load-bearing dependency now, not just a
documented endpoint — changing the stream response's framing, the `codec` query param's accepted
values, or removing the `api_key` fallback for this route would break playback in that client,
not just its own tests (of which there are none).

`decrypt.go` is **vendored, not imported**, from `zhaarey/apple-music-downloader`'s `utils/runv2` —
that project's module is literally named `main` and can't be imported normally. If upstream fixes a
bug in the cbcs decrypt logic, the fix has to be manually ported here, not pulled via `go get`.

Two independent caches, don't confuse their lifetimes: `masters` (resolved manifest URLs, 5 min TTL
— Apple's URLs are signed and expire) and the account-token cache in `apple/account.go` (5 min,
unrelated). Resolving a manifest on a cold cache round-trips to Apple and can take up to a minute;
`manifestTimeout` is 3 minutes for exactly that reason — don't shrink it without checking that
still holds.

`fetch()` retries a transient failure (network error, 429, 5xx) via the same `retry` package as
`catalog`, but **only** before any response bytes are read. Once `Open()` is streaming, the
audio asset's body is wrapped in `resumableReader`: a transport error or a premature EOF (before
the playlist's declared byte total) reconnects to the CDN with `Range: bytes=<pos>-` and
continues from the same offset (up to `assetResumeMaxRetries`), so a network blip against
Apple's edge no longer truncates the stream and make the client skip mid-song. A `416` on
resume means the asset is fully read (clean EOF). What `resumableReader` does **not** cover is a
failure inside the daemon's decrypt path (`decryptFragment`/`switchKeys`) — that's
track-specific and still ends the stream; re-decrypting from byte zero is not attempted, the
same reason a genuine seek is refused with `416` in `handleSongStream`.

**`handleSongStream`'s Range handling has one deliberate exception, and it's load-bearing**:
`Range: bytes=0-` is treated as no Range at all rather than refused. ffmpeg/libmpv's HTTP protocol
sends exactly that on *every* `open()` of a network stream, by default, to probe seekability — not
only on an actual seek — so refusing it (the original behavior) made this endpoint fail to open in
any native media player, elbert included, while curl without an explicit `Range` header worked
fine. `isWholeBodyRange` in [internal/api/stream.go](internal/api/stream.go) is the exact
allowlist: only `bytes=0-`, nothing else. Confirmed live: `curl -H "Range: bytes=0-" .../stream`
now 200s with the full body; `curl -H "Range: bytes=1000-"` still 416s. If you ever see "failed to
open"/"failed to open stream" from a real media player against an endpoint here that works fine
from curl with no headers, suspect exactly this pattern before anything else — curl doesn't probe
Range by default, real HTTP-based media players do.

---

## Downloads (`internal/download`)

`Manager.Run()` ([internal/download/manager.go](internal/download/manager.go)) drains one shared
queue with a single worker — **jobs run strictly one at a time**, by design: the daemon's decrypt
service handles one connection, so parallelism anywhere in this path buys nothing and would just
contend on the same socket. `Enqueue()` expands `song|album|artist|playlist` into a track-id list
(recursing into every album for `artist`) before creating the DB row, so a client can tell
immediately whether there's anything to download rather than waiting for the worker to find out.

Per-track pipeline, in order: `manifest` (resolve HLS) → `download`+`decrypt` (streamed together,
see `stream.Open`) → `remux` (ffmpeg stream-copy into a progressive `.m4a`, cover art attached in
the same pass) → `tag` → `done`. A job whose tracks partly fail still ends `done` with `error`
describing the count — only *every* track failing makes the job itself `failed`. `ResumeInterrupted`
runs once at startup and marks anything left `running`/`queued` from a crash as `failed`, so the
API never shows work that will never progress.

**A note for any client, elbert included**: Orchard's API *supports* `type: "playlist"` (it works,
and `docs/reference.md` documents it), but elbert's own UI deliberately never calls it, expanding a
playlist into one `type: "song"` job per track client-side instead, because
apple-music-downloader's pipeline (which this wraps) resolves and decrypts a playlist's tracks
more reliably one at a time than as one combined job. If you add another consumer of this API,
carry that same client-side choice forward rather than relying on the (functional but less
reliable) `type: playlist` path.

Files land at `<data>/library/<sanitised album>/<track number> - <sanitised title>.m4a` —
`sanitise()` strips filesystem-hostile characters and caps segment length at 120 bytes.
`fetchCover` and `fetchLyrics` are both best-effort (a missing cover or lyrics never fails the
track) — `fetchLyrics` writes a `.lrc` sidecar at the same base name, whatever tier
`catalog.ttmlToLRC` (below) landed on.

---

## Lyrics (`internal/catalog/lyrics.go`)

Apple's lyrics endpoint returns TTML; Orchard converts it to LRC server-side so no client has to
parse TTML itself, both from `catalog.Client.Lyrics()` (`GET /v1/songs/{id}/lyrics`) and from the
download pipeline's `.lrc` sidecar.

**The sync tier is read directly off the TTML, not guessed at.** Apple declares it on the root
element — `itunes:timing="Word" | "Line" | "None"` — confirmed against real responses: a `Line`
document has zero `<span>` elements; a `None` document's `<p>` elements carry no `begin`/`end`
attributes at all. `ttmlToLRC` cascades on that value — word → line → plain text — falling
through to the next tier down if a tier's own data turns out unusable (e.g. a `<p>` with no valid
timestamp), rather than failing outright.

Timestamps in Apple's TTML are **not** `HH:MM:SS` clock-time — they're abbreviated:
`"8.789"` (seconds only) or `"1:00.652"` (`M:SS.mmm`, minutes only present once ≥ 60s).
`parseTTMLTime` handles 1, 2 or 3 colon-separated parts; don't assume a fixed field count.

Word-level output is the informal "enhanced LRC" shape —
`[00:08.789]<00:08.789>Hello <00:09.100>world`, one inline `<mm:ss.xxx>` tag per word ahead of the
line's own bracket tag. This was validated against a structurally accurate **synthetic** sample
(Go's `encoding/xml` namespace-matching behaviour confirmed correct — a struct field tagged
`xml:"timing,attr"` with no namespace matches `itunes:timing` by local name alone); a real
word-by-word ("Apple Music Sing") track has not turned up through this endpoint in testing across
a couple dozen tracks, popular ones included — it may be gated to a narrower surface than the
general catalog lyrics call, or genuinely rare in the wild. If you ever find one, that's the case
most worth re-verifying by hand.

---

## Play activity (`internal/playactivity`)

The only thing in this codebase that **writes** to Apple on the user's behalf. `Client.Report`
POSTs one play event to `https://universal-activity-service.itunes.apple.com/play`, which is what
puts a track — and, more visibly, the album or playlist it came from — into the account's Recently
Played and into the personalization behind `/v1/me/recommendations`.

**The wire format was read off Apple's own published `musickit.js`**
(`js-cdn.music.apple.com/musickit/v3/musickit.js`), not reverse-engineered from traffic or guessed:
the `{client_id: "JSCLIENT", event_type: "JSPLAY", data: [...]}` envelope, the kebab-case event
fields, and every enum value (`event-type` 1 = PLAY_START / 0 = PLAY_END, `end-reason-type` 7 =
NATURAL_END_OF_TRACK, `source-type` 10 = MUSICKIT, `container-type` 3 = ALBUM / 2 = PLAYLIST, …)
come from that bundle's `MPAFTracker`/`PlayActivitySender`. If Apple reshapes the feed, that file
is where to re-read it — the field builders are named after the fields they emit
(`createFieldFn("end-reason-type", …)`), so they are greppable even minified.

Two things about the payload are load-bearing and easy to get wrong:

- **The container is what shows up, not the song.** `container-type` + `container-ids` (Apple names
  the id field per type: `album-adam-id`, `global-playlist-id`, `station-id`) are what make
  Recently Played list "that album" instead of a loose track. Confirmed live: one `start` event with
  an album container put the album at position 1 of `/v1/me/recent/played` on the very next request;
  a playlist container did the same for the playlist.
- **A `start` alone is enough** for Recently Played. The `end` event exists so the listen counts as
  a real measured play rather than just a visit; a client that only ever reports starts still gets
  the history behaviour users ask for.

Absent fields are omitted rather than zeroed — that is what MusicKit does (`createFieldFn`'s
`normalizeReturnValue` drops null), so don't "fill in" an unknown field with a default.

Unlike `catalog`, this package needs the numeric storefront (`143441-…`) on top of the two tokens,
which is why `apple.Manager` exposes a second, wider source (`PlayActivityTokens`) rather than
reusing `CatalogTokens`. Nothing here is on playback's critical path: a rejected report costs a
history entry and nothing else, and both `api` and elbert treat it that way.

---

## Store (`internal/store`)

SQLite via `modernc.org/sqlite` (pure Go, no cgo — keeps the Docker build simple). Three tables:
`jobs`, `job_tracks` (per-track progress, `ON DELETE CASCADE` from `jobs`), `tracks` (the
downloaded-file index, `album_id` populated from the song's catalog relationship where Apple
provides one — falls back to grouping by album name string for anything without it, see
`albumGroupKey` in [internal/store/jobs.go](internal/store/jobs.go)).

Migrations are embedded (`//go:embed migrations/*.sql`) and tracked in `schema_migrations`,
applied once each in a transaction, in filename order — add a new one as
`000N_description.sql`, never edit `0001_init.sql` in place.

`Tracks(ctx, TrackFilter)` is the richer-query surface added in phase 5: `since` (sync,
unchanged default ordering), `q` (LIKE search across title/artist/album, metacharacters escaped),
`artist`/`album`/`albumId`/`codec` exact filters, `sort`/`order`, `limit`/`offset`. `Albums()` and
`Artists()` are GROUP BY aggregates over the same table — `Albums()` labels a multi-artist group
`"Various Artists"`. None of these touch `jobs`/`job_tracks`.

---

## API (`internal/api`)

chi router; every `/v1` route requires `Authorization: Bearer <key>` (constant-time compare against
a SHA-256 digest, never the raw key — see `requireAPIKey` in
[internal/api/middleware.go](internal/api/middleware.go)) except `/healthz`. `presentedKey()` falls
back to an `?api_key=` query parameter whenever the header is absent — the doc comment motivates
this with the browser `EventSource` API (which can't set headers), but the fallback is not actually
restricted to SSE routes, it applies to every route. elbert's client relies on exactly that for
`/v1/songs/{id}/stream`, which a native media player hands a bare URL with no header support —
same embed-the-key-in-the-URL pattern Subsonic clients already use. Don't tighten this to
SSE-only without checking every consumer first. Rate limited
per-request via the same self-contained `ratelimit` package the catalog client uses, configured
separately (`ORCHARD_RATE_LIMIT`/`_BURST`, default 20/40) — a 429 here means *this* server's own
budget, not Apple's.

| File            | Owns                                                                          |
| ---------------- | ------------------------------------------------------------------------------ |
| `server.go`      | route table, `Server` struct wiring every dependency                          |
| `middleware.go`  | auth, rate limit, request logging (never logs headers or the query string)    |
| `apple.go`       | `/v1/apple/*` — status/login/2fa/logout                                       |
| `catalog.go`     | `/v1/storefront`, `/v1/search`, `/v1/albums,artists,playlists,songs/{id}...`, `/v1/stations/{id}/next-tracks` |
| `personal.go`    | `/v1/me/recommendations`, `/v1/me/recent/played`, `/v1/me/play-activity`      |
| `library_me.go`  | `/v1/me/library/{playlists,playlists/{id},songs,albums,artists,pins}`         |
| `stream.go`      | `/v1/songs/{id}/variants`, `/v1/songs/{id}/stream`                            |
| `downloads.go`   | `/v1/downloads*`, `/v1/tracks/{id}/file`, `/v1/library/*` (phase 5 queries)   |
| `errors.go`      | `writeError`/`writeJSON`/`decodeJSON` — the only place responses get built    |

Error mapping is centralised per concern (`catalogError` in catalog.go, `streamError` in
stream.go) rather than repeated per-handler — extend those switch statements when a new sentinel
error needs a specific status code, don't `writeError` an ad-hoc code inline in a handler.

Full request/response shapes, config table, and the security model live in
[docs/reference.md](docs/reference.md) — keep it in sync with `api/`, `catalog/models.go`, and
`config/config.go` whenever any of those change; it's the contract elbert's `orchard_service.dart`
is written against.

---

## Security posture (see docs/reference.md for the full list)

- `ORCHARD_API_KEY` is environment-only, never a flag — argv is world-readable via
  `/proc/<pid>/cmdline`, which is also exactly why the Apple password can't fully avoid the same
  exposure during `-L` (mitigated by restarting without it immediately after login).
- The daemon's four ports (10020/20020/30020/40020) are **unauthenticated** and must stay on
  `127.0.0.1` — anyone who can reach them has a live, unauthenticated Apple Music session.
- Downloads are SHA-256-verified against GitHub's published digest, not against a signature — pin
  `ORCHARD_WRAPPER_SHA256` if you don't trust the network path to GitHub for the initial fetch.
- The `chroot`+namespace the daemon uses is a *compatibility* shim (Docker blocks unshare/procfs
  by default otherwise), not a security boundary — it still has full network access and runs as
  your user. Never run Orchard as root.

---

## Known fragility

The daemon hooks a hardcoded address inside Apple's library (`libCoreLSKD+0x1d5709`) — it breaks
outright whenever Apple ships a new Music APK that shifts that offset. There is no way to detect
this in advance from Orchard's side; if catalog/streaming calls start failing after an upstream
`wrapper` release, pin a known-good build (`ORCHARD_WRAPPER_TAG` + `ORCHARD_WRAPPER_SHA256`) and
treat upgrading past it as a manual, tested step — never a routine bump.

`stream/decrypt.go` being vendored (not imported) means a cbcs decryption fix upstream in
`apple-music-downloader` does not arrive here automatically; check that file against upstream's
`utils/runv2` if playback/download starts producing corrupt audio after nothing here changed.
