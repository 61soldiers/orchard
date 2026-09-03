package catalog

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// ErrStationNotPlayable means Apple returned no tracks for a station.
//
// Apple has two very different things behind one `stations` resource type.
// A **personalized** station ("Discovery Station", an artist or song station,
// the "Stations for You" group) is really a rolling, server-generated queue of
// ordinary catalog songs, which is what StationTracks reads. A **live** one
// (Apple Música 1 and the other broadcast channels) is a continuous broadcast
// with no track list at all — there is nothing here to hand a player, and
// nothing in Orchard's stream pipeline that could play it if there were.
var ErrStationNotPlayable = errors.New("this station has no playable tracks")

// stationTracksMaxLimit is Apple's own hard cap on this endpoint. Asking for
// more is a 400 with "value must be an integer less than or equal to 10", not
// a silent clamp, so clamp here rather than passing a client's number through.
const stationTracksMaxLimit = 10

// StationTracks returns the next batch of songs for a personalized station,
// which is how a station is actually played: fetch a batch, play it, fetch the
// next when it runs low. Each call **advances** the station server-side, so
// two calls return different songs — it is not a stable track list, and paging
// backwards is not a thing.
//
// limit is clamped to [stationTracksMaxLimit]; zero or less omits it and lets
// Apple pick the batch size.
//
// The endpoint is `POST /v1/me/stations/next-tracks/{id}`, taken from Apple's
// own musickit.js (`MusicItemLoader.loadStationNextTracks`). POST is Apple's
// choice, not a write on the caller's behalf: advancing the station is the
// side effect of reading it.
//
// Non-song entries (music videos, which Orchard cannot play) are dropped, and
// a station that yields nothing playable returns [ErrStationNotPlayable].
func (c *Client) StationTracks(ctx context.Context, id string, limit int) ([]Song, error) {
	// No limit means "whatever Apple gives by default" — it is a station, the
	// batch size is Apple's business, and omitting the parameter is what its
	// own client does unless a caller has a reason to ask for a specific count.
	q := url.Values{}
	if limit > 0 {
		if limit > stationTracksMaxLimit {
			limit = stationTracksMaxLimit
		}
		q.Set("limit", strconv.Itoa(limit))
	}

	var out struct {
		Data []rawSong `json:"data"`
	}
	path := "/v1/me/stations/next-tracks/" + url.PathEscape(id)
	if err := c.post(ctx, path, q, &out); err != nil {
		return nil, err
	}

	songs := make([]Song, 0, len(out.Data))
	for _, r := range out.Data {
		if r.Type != "songs" {
			continue
		}
		songs = append(songs, convSong(r))
	}
	if len(songs) == 0 {
		return nil, ErrStationNotPlayable
	}
	return songs, nil
}
