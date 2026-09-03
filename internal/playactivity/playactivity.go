// Package playactivity reports finished and starting plays to Apple's
// play-activity service, so music played through Orchard shows up in the
// signed-in account's Recently Played (and feeds the personalization behind
// /v1/me/recommendations) exactly as if it had been played in Apple's own
// clients.
//
// This is the same feed Apple's web player writes to. MusicKit JS bundles a
// "MPAF" tracker that POSTs to universal-activity-service.itunes.apple.com
// with the developer + media-user tokens; the wire format implemented here —
// the {client_id, event_type, data[]} envelope, the kebab-case event fields
// and their enum values — was read off Apple's own published musickit.js
// (js-cdn.music.apple.com/musickit/v3/musickit.js), not guessed at. Orchard
// already holds both tokens, so a client never has to.
//
// Nothing here is on the critical path of playback: a rejected or dropped
// report is a lost history entry, never a failed stream.
package playactivity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchard/internal/ratelimit"
	"orchard/internal/retry"
)

const (
	endpoint = "https://universal-activity-service.itunes.apple.com/play"
	origin   = "https://music.apple.com"

	// clientID/eventType are the envelope's two constants. Apple's web player
	// sends exactly these; they identify the reporting client to the service.
	clientID  = "JSCLIENT"
	eventType = "JSPLAY"

	// sourceTypeMusicKit is PlayActivitySourceType.MUSICKIT — what MusicKit JS
	// itself reports. Orchard streams the same assets to the same account.
	sourceTypeMusicKit = 10

	// mediaTypeAudio is PlayActivityMediaType.AUDIO.
	mediaTypeAudio = 0

	// itemTypeStoreContent is PlayActivityItemType.ITUNES_STORE_CONTENT — a
	// regular catalog track, which is all Orchard can play.
	itemTypeStoreContent = 1
)

// Play activity event types.
const (
	eventPlayEnd   = 0
	eventPlayStart = 1
)

// Container types (PlayActivityContainerType).
const (
	containerUnknown  = 0
	containerRadio    = 1
	containerPlaylist = 2
	containerAlbum    = 3
	containerArtist   = 4
)

// Event reason hints, sent with a PLAY_START.
const (
	hintNotSpecified     = 0
	hintContainerChanged = 1
)

// ErrInvalidEvent means the caller described a play Apple would reject.
var ErrInvalidEvent = errors.New("invalid play activity event")

// TokenSource supplies everything a report needs to identify the account.
// It is deliberately the same shape as catalog.TokenSource plus the numeric
// storefront, so this package never has to know about apple.Manager.
type TokenSource func(ctx context.Context) (devToken, musicToken, storefrontID string, err error)

// EndReason is why a play stopped. The zero value is NOT_APPLICABLE, which is
// what a PLAY_START carries.
type EndReason int

// End reasons Apple recognises. Anything a music player can actually observe
// is here; the rest of Apple's enum is for surfaces Orchard has no equivalent
// of (ads, session timeouts, banned tracks).
const (
	EndNotApplicable    EndReason = 0
	EndOther            EndReason = 1
	EndSkippedForwards  EndReason = 2
	EndPaused           EndReason = 3
	EndManuallyReplaced EndReason = 5
	EndNatural          EndReason = 7
	EndFailedToLoad     EndReason = 10
	EndSkippedBackwards EndReason = 14
	EndExited           EndReason = 17
)

// endReasons maps the API's string form onto Apple's enum.
var endReasons = map[string]EndReason{
	"":                 EndNatural,
	"natural":          EndNatural,
	"other":            EndOther,
	"skipped_forward":  EndSkippedForwards,
	"skipped_backward": EndSkippedBackwards,
	"paused":           EndPaused,
	"replaced":         EndManuallyReplaced,
	"failed":           EndFailedToLoad,
	"exited":           EndExited,
}

// ParseEndReason resolves the API's string form. An unknown value is an error
// rather than a silent default, so a client typo does not quietly report every
// skip as a completed listen.
func ParseEndReason(s string) (EndReason, error) {
	r, ok := endReasons[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("%w: unknown end reason %q", ErrInvalidEvent, s)
	}
	return r, nil
}

// Container is what the track was played from — the album, playlist or artist
// page. Apple keys Recently Played on the *container*, so a play reported
// without one shows up as a bare song rather than as the album or playlist the
// listener actually opened.
type Container struct {
	// Kind is "album", "playlist", "artist" or "radio".
	Kind string `json:"kind"`
	// ID is the container's catalog id ("1440857781", "pl.u-xxxx", …).
	ID string `json:"id"`
}

func (c *Container) typeCode() int {
	if c == nil {
		return containerUnknown
	}
	switch strings.ToLower(c.Kind) {
	case "album", "albums":
		return containerAlbum
	case "playlist", "playlists":
		return containerPlaylist
	case "artist", "artists":
		return containerArtist
	case "radio", "station", "stations":
		return containerRadio
	default:
		return containerUnknown
	}
}

// ids builds the "container-ids" sub-object. Apple names the id field after
// the container type rather than using one generic key.
func (c *Container) ids() map[string]any {
	switch c.typeCode() {
	case containerAlbum:
		return map[string]any{"album-adam-id": c.ID}
	case containerPlaylist:
		return map[string]any{"global-playlist-id": c.ID}
	case containerRadio:
		return map[string]any{"station-id": c.ID}
	default:
		return nil
	}
}

// featureName labels the surface the play came from. Apple's own clients send
// a small vocabulary here; "music_kit-integration" is what MusicKit reports
// when it has no container.
func (c *Container) featureName() string {
	switch c.typeCode() {
	case containerAlbum:
		return "album"
	case containerPlaylist:
		return "playlist"
	case containerArtist:
		return "artist"
	case containerRadio:
		return "radio"
	default:
		return "music_kit-integration"
	}
}

// Event is one reportable moment in a track's playback.
type Event struct {
	// Start reports the beginning of a play; otherwise this is a PLAY_END.
	Start bool
	// SongID is the catalog adam id of the track.
	SongID string
	// DurationMs is the track's full length.
	DurationMs int
	// StartPositionMs is where this play segment began.
	StartPositionMs int
	// EndPositionMs is where it stopped. Ignored for a start event.
	EndPositionMs int
	// EndReason is why it stopped. Ignored for a start event.
	EndReason EndReason
	// Container is the album/playlist/artist the track was played from.
	Container *Container
}

func (e Event) validate() error {
	switch {
	case strings.TrimSpace(e.SongID) == "":
		return fmt.Errorf("%w: song id is required", ErrInvalidEvent)
	case len(e.SongID) > 64:
		return fmt.Errorf("%w: song id is too long", ErrInvalidEvent)
	case e.DurationMs < 0 || e.StartPositionMs < 0 || e.EndPositionMs < 0:
		return fmt.Errorf("%w: positions and duration cannot be negative", ErrInvalidEvent)
	}
	if e.Container != nil && strings.TrimSpace(e.Container.ID) == "" {
		return fmt.Errorf("%w: container id is required when a container is given", ErrInvalidEvent)
	}
	return nil
}

// Config tunes how the client paces itself against Apple.
type Config struct {
	RateLimit float64
	RateBurst int
}

// Client posts play activity as the signed-in account.
type Client struct {
	tokens  TokenSource
	hc      *http.Client
	limiter *ratelimit.Limiter
}

// New returns a Client that reports using tokens.
func New(tokens TokenSource, cfg Config) *Client {
	return &Client{
		tokens:  tokens,
		hc:      &http.Client{Timeout: 20 * time.Second},
		limiter: ratelimit.New(cfg.RateLimit, cfg.RateBurst),
	}
}

// APIError carries a rejection from the activity service.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("apple play activity: %s (%d)", e.Body, e.Status)
	}
	return fmt.Sprintf("apple play activity: status %d", e.Status)
}

// Report sends one event. Tokens are fetched once, outside the retry loop, for
// the same reason catalog.get does it: "session not ready" is not transient.
func (c *Client) Report(ctx context.Context, ev Event) error {
	if err := ev.validate(); err != nil {
		return err
	}

	dev, mut, storefront, err := c.tokens(ctx)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"client_id":  clientID,
		"event_type": eventType,
		"data":       []any{c.buildData(ev, dev, mut, storefront)},
	})
	if err != nil {
		return err
	}

	return retry.Do(ctx, retry.Default(), isRetryable, func() error {
		return c.post(ctx, body, dev, mut)
	})
}

// post sends the envelope. The tokens travel both inside the payload (Apple's
// own clients put them there) and as headers, the same pair every other Apple
// call uses.
func (c *Client) post(ctx context.Context, body []byte, dev, mut string) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Authorization", "Bearer "+dev)
	req.Header.Set("media-user-token", mut)

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
}

// buildData assembles one event object. Field names, enum values and the
// omit-when-absent behaviour mirror musickit.js's own PAF field builders.
func (c *Client) buildData(ev Event, dev, mut, storefront string) map[string]any {
	now := time.Now()
	_, offset := now.Zone()

	kind := eventPlayEnd
	if ev.Start {
		kind = eventPlayStart
	}

	data := map[string]any{
		"event-type":                     kind,
		"ids":                            map[string]any{"subscription-adam-id": ev.SongID},
		"type":                           itemTypeStoreContent,
		"media-type":                     mediaTypeAudio,
		"media-duration-in-milliseconds": ev.DurationMs,
		"start-position-in-milliseconds": ev.StartPositionMs,
		"developer-token":                dev,
		"user-token":                     mut,
		"store-front":                    storefront,
		"source-type":                    sourceTypeMusicKit,
		"feature-name":                   ev.Container.featureName(),
		"persistent-id":                  newUUID(),
		"timestamp":                      now.UnixMilli(),
		"utc-offset-in-seconds":          offset,
		"offline":                        false,
		"private-enabled":                false,
		"internal-build":                 false,
		"sb-enabled":                     true,
		"play-mode":                      map[string]any{"auto-play-mode": 0, "repeat-play-mode": 0, "shuffle-play-mode": 0},
	}

	if ct := ev.Container.typeCode(); ct != containerUnknown {
		data["container-type"] = ct
		if ids := ev.Container.ids(); ids != nil {
			data["container-ids"] = ids
		}
	}

	if ev.Start {
		// Apple only wants the hint on a start, and only cares whether the
		// listener moved to a different container than the previous track's.
		if ev.Container != nil {
			data["event-reason-hint-type"] = hintContainerChanged
		} else {
			data["event-reason-hint-type"] = hintNotSpecified
		}
		return data
	}

	end := ev.EndPositionMs
	if end == 0 {
		end = ev.StartPositionMs
	}
	data["end-position-in-milliseconds"] = end
	data["end-reason-type"] = int(ev.EndReason)
	// The envelope's per-event "milliseconds-since-play" is how long ago the
	// event happened; Orchard reports as it happens.
	data["milliseconds-since-play"] = 0
	return data
}

func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
	}
	// A transport failure; anything else (a validation error, a dead context)
	// short-circuits above this.
	return !errors.Is(err, ErrInvalidEvent) && !errors.Is(err, context.Canceled)
}

// newUUID returns a random RFC 4122 v4 UUID. Apple's clients send a fresh one
// per event; it only has to be unique, so there is no need for a dependency.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a timestamp keeps the field
		// present rather than sending a zero UUID.
		return fmt.Sprintf("%016x-0000-4000-8000-000000000000", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
