---
name: orchard-live-test
description: Rebuild orchard's Docker image from local source, restart the running container (the named volume keeps the signed-in Apple Music session, so no re-login is needed), wait for it to report healthy, then hit one or more endpoints with curl using the real API key from .env to verify a change against a live Apple Music account. Use whenever asked to "test orchard against the container", "verify the new endpoint works", "rebuild and test orchard", or after any change to internal/ or cmd/orchard that should be checked against a real logged-in account rather than just go build/go vet.
---

# orchard-live-test

Orchard has no `_test.go` files — the only real verification is running it against a live,
signed-in Apple Music account. This machine already has a container set up and logged in
(`docker compose up` once via `./setup.sh`), so testing a change means: rebuild the image from
the current source, restart the container, and hit the API with the real key — never re-running
setup or touching the Apple session.

Run every command from the `orchard` project root (`cd` there first if not already).

## 1. Confirm there's something to test against

```shell
docker compose ps
```

Expect one `orchard-orchard-1` (or similarly named) service. If nothing is running, this skill
still works — `docker compose up -d` will start it — but there may be no signed-in session yet;
check `state` in the status response in step 4 before assuming a catalog/personalization/download
endpoint should succeed. A `state` other than `"ready"` means don't debug the endpoint itself
first — sign-in status explains most failures.

Confirm an API key is on hand:

```shell
grep -oP '^ORCHARD_API_KEY=\K.*' .env
```

If `.env` is missing or empty, stop and ask the user — do not generate or write a new key
yourself; that key is also the elbert app's credential and replacing it silently would lock the
user's client out.

## 2. Rebuild and restart

```shell
docker compose build
docker compose up -d
```

`up -d` recreates the container from the freshly built image. This is safe to do at will: the
Apple session, the download library, and the SQLite DB all live in the named volume
(`orchard-data`, see `compose.yaml`), which a rebuild/recreate never touches. Expect a few seconds
of downtime while the old container stops and the new one starts — mention this if the user is
watching a client hit the server live, but don't ask permission first; restarting your own dev
container to verify your own change is the expected use of this skill.

## 3. Wait for healthy

```shell
for i in $(seq 1 20); do
  status=$(docker inspect --format='{{.State.Health.Status}}' orchard-orchard-1 2>/dev/null)
  [ "$status" = "healthy" ] && break
  sleep 2
done
echo "health: $status"
```

Adjust the container name if `docker compose ps` showed a different one. If it never reaches
`healthy`, check `docker compose logs --tail 50 orchard` before going further — a provisioning
failure (wrapper download/verify) or a config error will show there, and no endpoint will work
until it's resolved.

## 4. Verify against the real account

```shell
API_KEY=$(grep -oP '^ORCHARD_API_KEY=\K.*' .env)

curl -s -H "Authorization: Bearer $API_KEY" http://127.0.0.1:8080/v1/apple/status
```

Read `state` from the body, not the HTTP status — `ready` means signed in and the daemon's four
ports are up; anything else means auth/session-dependent endpoints (catalog, personalization,
streaming, downloads) will correctly 503 with `apple_not_ready` regardless of what you're
actually testing.

Then hit whatever the change touches, e.g.:

```shell
curl -s -H "Authorization: Bearer $API_KEY" http://127.0.0.1:8080/v1/me/recommendations | python3 -m json.tool | head -80
curl -s -H "Authorization: Bearer $API_KEY" "http://127.0.0.1:8080/v1/search?term=<query>&limit=5"
curl -s -o /tmp/resp.json -w "%{http_code}\n" -H "Authorization: Bearer $API_KEY" http://127.0.0.1:8080/v1/downloads
```

Save a response to a file before parsing it with `python3 -m json.tool`/`jq` rather than piping
`curl -s -w ...` straight into a JSON parser — the `-w` trailer text after the body breaks JSON
parsing (learned the hard way: `curl -s -w "\nHTTP %{http_code}\n" ... | python3 -c "json.load(...)"`
fails with "Extra data").

**Read the actual response shape before trusting an assumption about it.** Apple's own data does
not always match what a docstring or an earlier implementation assumed — e.g. `/v1/me/recommendations`
groups are not "one group per named mix": a live account showed multiple named playlists (`"New
Music"`, `"Get Up!"`, `"Chill"`, …) bundled under one umbrella group title like `"Playlists Made
for You"`. When a real response contradicts a comment or a piece of client code, fix the comment
and the client logic, not just the note-to-self — see the "Catalog" section of `CLAUDE.md` for the
last time this happened.

**A plain `curl` with no extra headers passing is not proof a streaming/media endpoint works.**
`/v1/songs/{id}/stream` streamed fine from bare `curl` — 200, full body — while every real media
player (elbert included, via `media_kit`/libmpv) failed to open the exact same URL. The cause: the
endpoint hard-refused *any* `Range` header with `416`, but ffmpeg/libmpv's HTTP protocol sends
`Range: bytes=0-` by default on every `open()`, not just an actual seek — curl only sends `Range`
if you ask it to. Reproduce what the real client does before ruling an endpoint "working":

```shell
curl -sv -o /dev/null -H "Range: bytes=0-" "$BASE_URL/v1/songs/<id>/stream?codec=alac&api_key=$API_KEY" 2>&1 | grep '^< HTTP'
```

If that 416s while the no-header request 200s, that's this exact bug (fixed in
`internal/api/stream.go`'s `isWholeBodyRange` — `bytes=0-` is now treated as no Range at all,
since it's semantically "everything, from the start", exactly what a 200 already provides; a
genuine seek like `bytes=1000-` still correctly 416s).

**A container recreate can leave the Apple session flaky for the first request or two.** After a
`docker compose build && up -d`, `/v1/apple/status` can report `ready` while the wrapper daemon is
still settling — the first real manifest/stream request afterward may hit `"read manifest URL:
EOF"` followed by `"the wrapper daemon exited unexpectedly"` in the logs, dropping the session to
`failed`. `docker compose restart orchard` recovers it from the persisted session (no re-login
needed) — wait for `healthy` again, then retry. Don't mistake this for a regression in whatever
you were actually testing; confirm status is `ready` again before deciding a fix didn't work.

## 5. Clean up

```shell
rm -f /tmp/resp.json  # or whatever scratch files step 4 produced
```

Leave the container running afterward — don't `docker compose down` or `down -v` (`-v` deletes the
volume, including the Apple session) unless the user explicitly asks for a clean slate.
