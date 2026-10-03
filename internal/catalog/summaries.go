package catalog

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// summariesBase is Apple's Replay collection. Like /v1/me/library/pins it is
// not part of MusicKit's documented surface — every shape below was read off a
// live account — so treat it as observed rather than promised. What it is,
// though, is the same data music.apple.com/replay renders: listening totals
// and rankings Apple computes across all of the account's devices, which is
// why Orchard exposes it instead of a client trying to count plays itself.
const summariesBase = "/v1/me/music-summaries"

// PeriodYear and PeriodMonth are the two granularities Apple computes. A
// month request needs a year alongside it; a year request must not carry one
// (Apple rejects both mistakes with a 400).
const (
	PeriodYear  = "year"
	PeriodMonth = "month"
)

// ErrInvalidPeriod is returned for a period Apple doesn't compute.
var ErrInvalidPeriod = errors.New("period must be \"year\" or \"month\"")

// Summary is one Replay period — a year, or a month within one.
//
// The counters are always present; the Top* lists and Milestones are filled
// only by Summary(), since Apple returns them as "views" that the collection
// listing doesn't carry.
type Summary struct {
	// ID is Apple's own summary id, "year-2025" or "month-2026-8". It is what
	// Summary() takes.
	ID     string `json:"id"`
	Period string `json:"period"`
	// Name is Apple's label for the period: "2025" for a year, "August" for a
	// month — already localized by Apple, so a client shows it as-is.
	Name  string `json:"name"`
	Year  int    `json:"year"`
	Month int    `json:"month,omitempty"`

	ListenTimeInMinutes int `json:"listenTimeInMinutes"`
	UniqueSongCount     int `json:"uniqueSongCount,omitempty"`
	UniqueAlbumCount    int `json:"uniqueAlbumCount,omitempty"`
	UniqueArtistCount   int `json:"uniqueArtistCount,omitempty"`
	UniqueGenreCount    int `json:"uniqueGenreCount,omitempty"`
	UniquePlaylistCount int `json:"uniquePlaylistCount,omitempty"`
	UniqueStationCount  int `json:"uniqueStationCount,omitempty"`

	// PlaylistID is the catalog id of Apple's generated "Replay Your Top Songs
	// of <year>" playlist for this period, playable like any other playlist.
	PlaylistID   string `json:"playlistId,omitempty"`
	PlaylistName string `json:"playlistName,omitempty"`

	TopSongs     []SummaryEntry `json:"topSongs,omitempty"`
	TopAlbums    []SummaryEntry `json:"topAlbums,omitempty"`
	TopArtists   []SummaryEntry `json:"topArtists,omitempty"`
	TopGenres    []SummaryEntry `json:"topGenres,omitempty"`
	TopPlaylists []SummaryEntry `json:"topPlaylists,omitempty"`
	TopStations  []SummaryEntry `json:"topStations,omitempty"`
	Milestones   []Milestone    `json:"milestones,omitempty"`
}

// SummaryEntry is one ranked row of a Replay period, in Apple's own order
// (most played first).
//
// Not every rank carries every number. Apple reports ListenTimeInMinutes for
// albums, artists, genres, playlists and stations but *not* for songs, where
// only PlayCount exists — a client showing minutes per song would have to
// multiply by the track length itself, which is why this passes Apple's zero
// through rather than inventing one.
type SummaryEntry struct {
	PlayCount           int `json:"playCount"`
	ListenTimeInMinutes int `json:"listenTimeInMinutes,omitempty"`

	FirstPlayed string `json:"firstPlayed,omitempty"`
	LastPlayed  string `json:"lastPlayed,omitempty"`

	// Item is the resource ranked — a song, album, artist, playlist or
	// station, carrying the catalog id a client opens or plays. A genre has
	// no resource of its own: those entries carry Name and nothing else.
	Item *SummaryItem `json:"item,omitempty"`
	// Name is the row's display name whatever the kind, so a client never has
	// to branch on Item being nil.
	Name string `json:"name"`
}

// SummaryItem is the catalog resource behind a ranked row. It is deliberately
// narrower than Song/Album/Artist: a Replay list is a leaderboard, and a
// client that opens a row fetches the full resource by id anyway.
type SummaryItem struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"` // songs | albums | artists | playlists | stations
	Name        string   `json:"name"`
	ArtistName  string   `json:"artistName,omitempty"`
	AlbumName   string   `json:"albumName,omitempty"`
	CuratorName string   `json:"curatorName,omitempty"`
	DurationMs  int      `json:"durationMs,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	Artwork     *Artwork `json:"artwork,omitempty"`
}

// Milestone is one "you passed N minutes / N songs" badge Apple awards within
// a period. ArtworkLight/ArtworkDark are Apple's own generated badge images,
// templated on {w}/{h} like every other artwork URL — and unlike every other
// one, they come as a light/dark pair, so a client picks by theme.
type Milestone struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // listen-time | song | artist | album | genre
	// Value is the threshold reached, as Apple formats it ("10000").
	Value               string `json:"value,omitempty"`
	DateReached         string `json:"dateReached,omitempty"`
	Status              string `json:"status,omitempty"`
	ListenTimeInMinutes int    `json:"listenTimeInMinutes,omitempty"`
	ArtworkLight        string `json:"artworkLight,omitempty"`
	ArtworkDark         string `json:"artworkDark,omitempty"`
}

// SummaryIndex is the listing of the periods Apple has computed, newest
// first, plus the meta the Replay page itself keys off.
type SummaryIndex struct {
	Summaries []Summary `json:"summaries"`
	// LatestYear is the most recent year with any data, and IsEndOfYear
	// reports whether Apple considers the current year's Replay final.
	LatestYear  int  `json:"latestYear,omitempty"`
	IsEndOfYear bool `json:"isEndOfYear"`
}

// summaryViews are every view Apple computes for a period. They are requested
// together because the cost is one request either way and a Replay screen
// shows all of them.
const summaryViews = "top-songs,top-albums,top-artists,top-genres," +
	"top-playlists,top-stations,milestones"

// summaryIncludes hydrates each ranked row's resource inline. Without them
// Apple returns bare ids, and a leaderboard of 100 ids would cost 100 catalog
// lookups to render. The fields[] narrowing is what keeps that one response
// from carrying every album's editorial notes and discography.
func summaryIncludes() url.Values {
	return url.Values{
		"include[song-period-summaries]":     {"song"},
		"include[album-period-summaries]":    {"album"},
		"include[artist-period-summaries]":   {"artist"},
		"include[playlist-period-summaries]": {"playlist"},
		"include[station-period-summaries]":  {"station"},
		"fields[songs]":                      {"name,artistName,albumName,artwork,durationInMillis,genreNames"},
		"fields[albums]":                     {"name,artistName,artwork,genreNames"},
		"fields[artists]":                    {"name,artwork,genreNames"},
		"fields[playlists]":                  {"name,curatorName,artwork"},
		"fields[stations]":                   {"name,artwork,isLive"},
	}
}

// Summaries lists the Replay periods Apple has computed. period is "year" for
// every year on record, or "month" together with a year for that year's
// months. Apple rejects a month request without a year and a year request
// with one, so the pairing is enforced here rather than surfaced as a 400.
func (c *Client) Summaries(ctx context.Context, period string, year int) (*SummaryIndex, error) {
	q := url.Values{}
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", PeriodYear:
		q.Set("period", PeriodYear)
	case PeriodMonth:
		if year <= 0 {
			return nil, errors.New("a year is required for monthly summaries")
		}
		q.Set("period", PeriodMonth)
		q.Set("year", strconv.Itoa(year))
	default:
		return nil, ErrInvalidPeriod
	}

	var raw struct {
		Data []rawSummary `json:"data"`
		Meta struct {
			LatestYear  int  `json:"latestYear"`
			IsEndOfYear bool `json:"isEndOfYear"`
		} `json:"meta"`
	}
	if err := c.get(ctx, summariesBase+"/search", q, &raw); err != nil {
		return nil, err
	}

	out := &SummaryIndex{
		Summaries:   make([]Summary, 0, len(raw.Data)),
		LatestYear:  raw.Meta.LatestYear,
		IsEndOfYear: raw.Meta.IsEndOfYear,
	}
	for _, r := range raw.Data {
		out.Summaries = append(out.Summaries, convSummary(r))
	}
	return out, nil
}

// Summary returns one period with every ranking Apple computed for it. id is
// a Summary.ID from Summaries — "year-2025" or "month-2026-8".
func (c *Client) Summary(ctx context.Context, id string) (*Summary, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("id is required")
	}
	if strings.ContainsAny(id, "/?#") {
		return nil, ErrNotFound
	}

	q := summaryIncludes()
	q.Set("views", summaryViews)

	var raw struct {
		Data []rawSummary `json:"data"`
	}
	if err := c.get(ctx, summariesBase+"/"+url.PathEscape(id), q, &raw); err != nil {
		return nil, err
	}
	if len(raw.Data) == 0 {
		return nil, ErrNotFound
	}
	s := convSummary(raw.Data[0])
	dropYearScopedRankings(&s)
	return &s, nil
}

// dropYearScopedRankings clears a month's Top* lists, because Apple does not
// compute them.
//
// Asking for a month returns the *year's* 100 rows: same entries as year-YYYY,
// each carrying `year` and a year-wide firstPlayed/lastPlayed, with only the
// row ids rewritten to the month (`month-2025-10-song-1773474484`). Verified
// live against both `?views=top-songs` and `/view/top-songs`; `period=`,
// `year=` and `filter[period]=` are all rejected on the per-id call, and the
// listing's `topSongCount: 30` has no endpoint that will produce those 30
// rows. See the Replay notes in CLAUDE.md.
//
// Passing them through is what makes a client render a year's leaderboard
// under a month's heading — which elbert's Replay month page did. A month's
// own counters (listen time, unique counts, milestones) are genuinely the
// month's and are left alone.
func dropYearScopedRankings(s *Summary) {
	if s.Period != PeriodMonth {
		return
	}
	s.TopSongs = nil
	s.TopAlbums = nil
	s.TopArtists = nil
	s.TopGenres = nil
	s.TopPlaylists = nil
	s.TopStations = nil
}

type rawSummary struct {
	ID         string `json:"id"`
	Attributes struct {
		Name                string `json:"name"`
		Period              string `json:"period"`
		Year                int    `json:"year"`
		Month               int    `json:"month"`
		ListenTimeInMinutes int    `json:"listenTimeInMinutes"`
		UniqueSongCount     int    `json:"uniqueSongCount"`
		UniqueAlbumCount    int    `json:"uniqueAlbumCount"`
		UniqueArtistCount   int    `json:"uniqueArtistCount"`
		UniqueGenreCount    int    `json:"uniqueGenreCount"`
		UniquePlaylistCount int    `json:"uniquePlaylistCount"`
		UniqueStationCount  int    `json:"uniqueStationCount"`
	} `json:"attributes"`
	Relationships struct {
		Playlist struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Meta struct {
				Title string `json:"title"`
			} `json:"meta"`
		} `json:"playlist"`
	} `json:"relationships"`
	Views map[string]rawSummaryView `json:"views"`
}

type rawSummaryView struct {
	Data []rawSummaryEntry `json:"data"`
}

// rawSummaryEntry covers every *-period-summaries row with one struct: the
// attributes differ only in which of them Apple bothers to send, and the
// single hydrated relationship is whichever one matches the row's type.
type rawSummaryEntry struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		PlayCount           int    `json:"playCount"`
		ListenTimeInMinutes int    `json:"listenTimeInMinutes"`
		FirstPlayed         string `json:"firstPlayed"`
		LastPlayed          string `json:"lastPlayed"`
		GenreName           string `json:"genreName"`
		// Milestone-only attributes.
		Kind         string `json:"kind"`
		Value        string `json:"value"`
		DateReached  string `json:"dateReached"`
		Status       string `json:"status"`
		MilestoneArt *struct {
			LightFlavor struct {
				URL string `json:"url"`
			} `json:"lightFlavor"`
			DarkFlavor struct {
				URL string `json:"url"`
			} `json:"darkFlavor"`
		} `json:"artwork"`
	} `json:"attributes"`
	Relationships map[string]struct {
		Data []rawSummaryResource `json:"data"`
	} `json:"relationships"`
}

type rawSummaryResource struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Attributes struct {
		Name             string      `json:"name"`
		ArtistName       string      `json:"artistName"`
		AlbumName        string      `json:"albumName"`
		CuratorName      string      `json:"curatorName"`
		DurationInMillis int         `json:"durationInMillis"`
		GenreNames       []string    `json:"genreNames"`
		Artwork          *rawArtwork `json:"artwork"`
	} `json:"attributes"`
}

func convSummary(r rawSummary) Summary {
	a := r.Attributes
	s := Summary{
		ID:                  r.ID,
		Period:              a.Period,
		Name:                a.Name,
		Year:                a.Year,
		Month:               a.Month,
		ListenTimeInMinutes: a.ListenTimeInMinutes,
		UniqueSongCount:     a.UniqueSongCount,
		UniqueAlbumCount:    a.UniqueAlbumCount,
		UniqueArtistCount:   a.UniqueArtistCount,
		UniqueGenreCount:    a.UniqueGenreCount,
		UniquePlaylistCount: a.UniquePlaylistCount,
		UniqueStationCount:  a.UniqueStationCount,
		PlaylistName:        r.Relationships.Playlist.Meta.Title,
	}
	if d := r.Relationships.Playlist.Data; len(d) > 0 {
		s.PlaylistID = d[0].ID
	}

	s.TopSongs = convSummaryEntries(r.Views["top-songs"])
	s.TopAlbums = convSummaryEntries(r.Views["top-albums"])
	s.TopArtists = convSummaryEntries(r.Views["top-artists"])
	s.TopGenres = convSummaryEntries(r.Views["top-genres"])
	s.TopPlaylists = convSummaryEntries(r.Views["top-playlists"])
	s.TopStations = convSummaryEntries(r.Views["top-stations"])
	s.Milestones = convMilestones(r.Views["milestones"])
	return s
}

func convSummaryEntries(v rawSummaryView) []SummaryEntry {
	if len(v.Data) == 0 {
		return nil
	}
	out := make([]SummaryEntry, 0, len(v.Data))
	for _, r := range v.Data {
		e := SummaryEntry{
			PlayCount:           r.Attributes.PlayCount,
			ListenTimeInMinutes: r.Attributes.ListenTimeInMinutes,
			FirstPlayed:         r.Attributes.FirstPlayed,
			LastPlayed:          r.Attributes.LastPlayed,
			Name:                r.Attributes.GenreName,
		}
		if item := convSummaryItem(r); item != nil {
			e.Item = item
			e.Name = item.Name
		}
		// A row Apple sent with neither a resource nor a name is a rank of
		// nothing — drop it rather than render a blank line.
		if e.Name == "" {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// convSummaryItem picks the one hydrated resource out of a row. Apple names
// the relationship after the type ("song", "album", …) and sends exactly one,
// so this takes the first it finds rather than switching on the row's type.
func convSummaryItem(r rawSummaryEntry) *SummaryItem {
	for _, rel := range r.Relationships {
		for _, res := range rel.Data {
			if res.ID == "" {
				continue
			}
			a := res.Attributes
			return &SummaryItem{
				ID:          res.ID,
				Type:        res.Type,
				Name:        a.Name,
				ArtistName:  a.ArtistName,
				AlbumName:   a.AlbumName,
				CuratorName: a.CuratorName,
				DurationMs:  a.DurationInMillis,
				Genres:      a.GenreNames,
				Artwork:     convArtwork(a.Artwork),
			}
		}
	}
	return nil
}

func convMilestones(v rawSummaryView) []Milestone {
	if len(v.Data) == 0 {
		return nil
	}
	out := make([]Milestone, 0, len(v.Data))
	for _, r := range v.Data {
		a := r.Attributes
		m := Milestone{
			ID:                  r.ID,
			Kind:                a.Kind,
			Value:               a.Value,
			DateReached:         a.DateReached,
			Status:              a.Status,
			ListenTimeInMinutes: a.ListenTimeInMinutes,
		}
		if art := a.MilestoneArt; art != nil {
			m.ArtworkLight = art.LightFlavor.URL
			m.ArtworkDark = art.DarkFlavor.URL
		}
		if m.Kind == "" {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
