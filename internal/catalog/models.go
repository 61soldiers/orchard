package catalog

import (
	"encoding/json"
	"strings"
)

// Artwork is Apple's image reference. URL is a template containing {w} and {h}.
type Artwork struct {
	URL      string `json:"url"`
	ThumbURL string `json:"thumbUrl,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	BGColor  string `json:"bgColor,omitempty"`
}

// Quality flags derived from Apple's audioTraits.
type Quality struct {
	Lossless bool `json:"lossless"`
	HiRes    bool `json:"hiRes"`
	Atmos    bool `json:"atmos"`
	Spatial  bool `json:"spatial"`
}

// Song is a catalog track.
type Song struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ArtistName    string   `json:"artistName"`
	AlbumID       string   `json:"albumId,omitempty"`
	AlbumName     string   `json:"albumName,omitempty"`
	ComposerName  string   `json:"composerName,omitempty"`
	DiscNumber    int      `json:"discNumber,omitempty"`
	TrackNumber   int      `json:"trackNumber,omitempty"`
	DurationMs    int      `json:"durationMs,omitempty"`
	ISRC          string   `json:"isrc,omitempty"`
	ReleaseDate   string   `json:"releaseDate,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	ContentRating string   `json:"contentRating,omitempty"`
	HasLyrics     bool     `json:"hasLyrics"`
	HasSyncLyrics bool     `json:"hasTimeSyncedLyrics"`
	Quality       Quality  `json:"quality"`
	Artwork       *Artwork `json:"artwork,omitempty"`
}

// Album is a catalog album. Tracks is populated by Album(), not by search.
type Album struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ArtistName    string   `json:"artistName"`
	TrackCount    int      `json:"trackCount,omitempty"`
	ReleaseDate   string   `json:"releaseDate,omitempty"`
	RecordLabel   string   `json:"recordLabel,omitempty"`
	Copyright     string   `json:"copyright,omitempty"`
	UPC           string   `json:"upc,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	ContentRating string   `json:"contentRating,omitempty"`
	IsSingle      bool     `json:"isSingle,omitempty"`
	IsCompilation bool     `json:"isCompilation,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	Quality       Quality  `json:"quality"`
	Artwork       *Artwork `json:"artwork,omitempty"`
	Tracks        []Song   `json:"tracks,omitempty"`
}

// Artist is a catalog artist. Albums is populated by Artist().
type Artist struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Genres  []string `json:"genres,omitempty"`
	Notes   string   `json:"notes,omitempty"`
	Artwork *Artwork `json:"artwork,omitempty"`
	Albums  []Album  `json:"albums,omitempty"`
}

// Playlist is a catalog playlist. Tracks is populated by Playlist().
type Playlist struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	CuratorName  string   `json:"curatorName,omitempty"`
	Description  string   `json:"description,omitempty"`
	PlaylistType string   `json:"playlistType,omitempty"`
	LastModified string   `json:"lastModified,omitempty"`
	Artwork      *Artwork `json:"artwork,omitempty"`
	Tracks       []Song   `json:"tracks,omitempty"`
}

// SearchResults holds whatever types the caller asked for.
type SearchResults struct {
	Songs     []Song     `json:"songs,omitempty"`
	Albums    []Album    `json:"albums,omitempty"`
	Artists   []Artist   `json:"artists,omitempty"`
	Playlists []Playlist `json:"playlists,omitempty"`
}

// Lyrics is a single lyrics document. Apple returns TTML.
type Lyrics struct {
	SongID string `json:"songId"`
	Format string `json:"format"`
	TTML   string `json:"ttml"`
	// LRC is TTML converted to the best sync tier available — word-by-word,
	// falling back to line-level, falling back to plain text with no time
	// tags — so a client that just wants a lyrics file never has to parse
	// TTML itself. Empty when conversion failed; TTML is still returned in
	// that case.
	LRC string `json:"lrc,omitempty"`
	// SyncLevel is "word", "line" or "none", naming whichever tier LRC
	// actually landed on.
	SyncLevel string `json:"syncLevel,omitempty"`
}

// Station is a Apple Music radio station. Orchard cannot stream or download
// one — its pipeline only decrypts a song's HLS asset — so a station is
// surfaced for display only.
type Station struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Artwork *Artwork `json:"artwork,omitempty"`
	IsLive  bool     `json:"isLive,omitempty"`
}

// Item is a minimal, uniform view of a catalog resource: a song, album,
// playlist or station. It exists because Apple's personalization endpoints
// mix resource types in one list (recommendations, recently played); a
// caller that wants the full resource fetches it by ID and Type through the
// regular catalog endpoints.
type Item struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"` // songs | albums | playlists | stations
	Name        string   `json:"name"`
	ArtistName  string   `json:"artistName,omitempty"`
	CuratorName string   `json:"curatorName,omitempty"`
	Artwork     *Artwork `json:"artwork,omitempty"`
}

// RecommendationGroup is one row of Apple's "Made For You" personalization.
// Confirmed against a live account: Apple bundles several named playlists —
// "New Music", "Heavy Rotation", "Your Essentials", "Get Up!", "Chill" — into
// one umbrella group (e.g. "Playlists Made for You"), rather than giving each
// its own group. A group's Items can also mix in albums and stations
// (Recently Played, Stations for You), so this is not "one group per mix".
type RecommendationGroup struct {
	ID     string `json:"id"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason,omitempty"`
	Items  []Item `json:"items"`
}

// rawDisplayString is Apple's shape for a localized, pre-formatted label,
// used by the recommendations endpoint's title/reason fields.
type rawDisplayString struct {
	StringForDisplay string `json:"stringForDisplay"`
}

type rawStationAttrs struct {
	Name    string      `json:"name"`
	IsLive  bool        `json:"isLive"`
	Artwork *rawArtwork `json:"artwork"`
}

// rawMixedResource is one entry in a heterogeneous Apple list (recently
// played, a recommendation group's contents), where Attributes' shape
// depends on Type and is decoded lazily by toItem.
type rawMixedResource struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Attributes json.RawMessage `json:"attributes"`
}

// toItem normalises one mixed resource into an Item, reporting false for a
// type it does not know how to display (Apple adds new resource types over
// time; skipping one beats failing the whole list).
func toItem(r rawMixedResource) (Item, bool) {
	it := Item{ID: r.ID, Type: r.Type}
	switch r.Type {
	case "songs":
		var a rawSongAttrs
		if json.Unmarshal(r.Attributes, &a) != nil {
			return Item{}, false
		}
		it.Name, it.ArtistName, it.Artwork = a.Name, a.ArtistName, convArtwork(a.Artwork)
	case "albums":
		var a rawAlbumAttrs
		if json.Unmarshal(r.Attributes, &a) != nil {
			return Item{}, false
		}
		it.Name, it.ArtistName, it.Artwork = a.Name, a.ArtistName, convArtwork(a.Artwork)
	case "playlists":
		var a rawPlaylistAttrs
		if json.Unmarshal(r.Attributes, &a) != nil {
			return Item{}, false
		}
		it.Name, it.CuratorName, it.Artwork = a.Name, a.CuratorName, convArtwork(a.Artwork)
	case "stations":
		var a rawStationAttrs
		if json.Unmarshal(r.Attributes, &a) != nil {
			return Item{}, false
		}
		it.Name, it.Artwork = a.Name, convArtwork(a.Artwork)
	default:
		return Item{}, false
	}
	return it, true
}

// --- raw Apple shapes -------------------------------------------------------

type rawArtwork struct {
	URL     string `json:"url"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	BGColor string `json:"bgColor"`
}

type rawNotes struct {
	Short    string `json:"short"`
	Standard string `json:"standard"`
}

type rawSongAttrs struct {
	Name                string      `json:"name"`
	ArtistName          string      `json:"artistName"`
	AlbumName           string      `json:"albumName"`
	ComposerName        string      `json:"composerName"`
	DiscNumber          int         `json:"discNumber"`
	TrackNumber         int         `json:"trackNumber"`
	DurationInMillis    int         `json:"durationInMillis"`
	ISRC                string      `json:"isrc"`
	ReleaseDate         string      `json:"releaseDate"`
	GenreNames          []string    `json:"genreNames"`
	ContentRating       string      `json:"contentRating"`
	HasLyrics           bool        `json:"hasLyrics"`
	HasTimeSyncedLyrics bool        `json:"hasTimeSyncedLyrics"`
	AudioTraits         []string    `json:"audioTraits"`
	Artwork             *rawArtwork `json:"artwork"`
}

type rawAlbumAttrs struct {
	Name           string      `json:"name"`
	ArtistName     string      `json:"artistName"`
	TrackCount     int         `json:"trackCount"`
	ReleaseDate    string      `json:"releaseDate"`
	RecordLabel    string      `json:"recordLabel"`
	Copyright      string      `json:"copyright"`
	UPC            string      `json:"upc"`
	GenreNames     []string    `json:"genreNames"`
	ContentRating  string      `json:"contentRating"`
	IsSingle       bool        `json:"isSingle"`
	IsCompilation  bool        `json:"isCompilation"`
	AudioTraits    []string    `json:"audioTraits"`
	EditorialNotes *rawNotes   `json:"editorialNotes"`
	Artwork        *rawArtwork `json:"artwork"`
}

type rawArtistAttrs struct {
	Name           string      `json:"name"`
	GenreNames     []string    `json:"genreNames"`
	EditorialNotes *rawNotes   `json:"editorialNotes"`
	Artwork        *rawArtwork `json:"artwork"`
}

type rawPlaylistAttrs struct {
	Name             string      `json:"name"`
	CuratorName      string      `json:"curatorName"`
	PlaylistType     string      `json:"playlistType"`
	LastModifiedDate string      `json:"lastModifiedDate"`
	Description      *rawNotes   `json:"description"`
	EditorialNotes   *rawNotes   `json:"editorialNotes"`
	Artwork          *rawArtwork `json:"artwork"`
}

// rawSong carries its albums relationship: Apple includes the related
// resource's id by default, without needing an "include" query parameter.
type rawSong struct {
	ID            string       `json:"id"`
	Type          string       `json:"type"`
	Attributes    rawSongAttrs `json:"attributes"`
	Relationships struct {
		Albums struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"albums"`
	} `json:"relationships"`
}

type rawAlbum struct {
	ID            string        `json:"id"`
	Attributes    rawAlbumAttrs `json:"attributes"`
	Relationships struct {
		Tracks struct {
			Data []rawSong `json:"data"`
		} `json:"tracks"`
	} `json:"relationships"`
}

type rawArtist struct {
	ID         string         `json:"id"`
	Attributes rawArtistAttrs `json:"attributes"`
	Views      struct {
		FullAlbums struct {
			Data []rawAlbum `json:"data"`
		} `json:"full-albums"`
	} `json:"views"`
}

type rawPlaylist struct {
	ID            string           `json:"id"`
	Attributes    rawPlaylistAttrs `json:"attributes"`
	Relationships struct {
		Tracks struct {
			Data []rawSong `json:"data"`
			Next string    `json:"next"`
		} `json:"tracks"`
	} `json:"relationships"`
}

type rawSearchResponse struct {
	Results struct {
		Songs struct {
			Data []rawSong `json:"data"`
		} `json:"songs"`
		Albums struct {
			Data []rawAlbum `json:"data"`
		} `json:"albums"`
		Artists struct {
			Data []rawArtist `json:"data"`
		} `json:"artists"`
		Playlists struct {
			Data []rawPlaylist `json:"data"`
		} `json:"playlists"`
	} `json:"results"`
}

// --- conversion -------------------------------------------------------------

// thumbSize is the resolved artwork size handed to clients alongside the
// template, so a player can show something without templating the URL itself.
const thumbSize = 600

func convArtwork(a *rawArtwork) *Artwork {
	if a == nil || a.URL == "" {
		return nil
	}
	out := &Artwork{URL: a.URL, Width: a.Width, Height: a.Height, BGColor: a.BGColor}
	out.ThumbURL = ArtworkURL(a.URL, thumbSize, thumbSize)
	return out
}

// ArtworkURL fills Apple's {w}/{h} artwork template.
func ArtworkURL(template string, w, h int) string {
	if template == "" {
		return ""
	}
	r := strings.NewReplacer("{w}", itoa(w), "{h}", itoa(h))
	return r.Replace(template)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func convQuality(traits []string) Quality {
	var q Quality
	for _, t := range traits {
		switch t {
		case "lossless":
			q.Lossless = true
		case "hi-res-lossless":
			q.Lossless, q.HiRes = true, true
		case "atmos":
			q.Atmos = true
		case "spatial":
			q.Spatial = true
		}
	}
	return q
}

func notesText(n *rawNotes) string {
	if n == nil {
		return ""
	}
	if n.Standard != "" {
		return n.Standard
	}
	return n.Short
}

func convSong(r rawSong) Song {
	a := r.Attributes
	var albumID string
	if data := r.Relationships.Albums.Data; len(data) > 0 {
		albumID = data[0].ID
	}
	return Song{
		ID: r.ID, Name: a.Name, ArtistName: a.ArtistName,
		AlbumID: albumID, AlbumName: a.AlbumName,
		ComposerName: a.ComposerName, DiscNumber: a.DiscNumber, TrackNumber: a.TrackNumber,
		DurationMs: a.DurationInMillis, ISRC: a.ISRC, ReleaseDate: a.ReleaseDate,
		Genres: a.GenreNames, ContentRating: a.ContentRating,
		HasLyrics: a.HasLyrics, HasSyncLyrics: a.HasTimeSyncedLyrics,
		Quality: convQuality(a.AudioTraits), Artwork: convArtwork(a.Artwork),
	}
}

func convAlbum(r rawAlbum) Album {
	a := r.Attributes
	out := Album{
		ID: r.ID, Name: a.Name, ArtistName: a.ArtistName, TrackCount: a.TrackCount,
		ReleaseDate: a.ReleaseDate, RecordLabel: a.RecordLabel, Copyright: a.Copyright,
		UPC: a.UPC, Genres: a.GenreNames, ContentRating: a.ContentRating,
		IsSingle: a.IsSingle, IsCompilation: a.IsCompilation, Notes: notesText(a.EditorialNotes),
		Quality: convQuality(a.AudioTraits), Artwork: convArtwork(a.Artwork),
	}
	for _, t := range r.Relationships.Tracks.Data {
		if t.Type == "songs" {
			out.Tracks = append(out.Tracks, convSong(t))
		}
	}
	return out
}

func convArtist(r rawArtist) Artist {
	a := r.Attributes
	out := Artist{
		ID: r.ID, Name: a.Name, Genres: a.GenreNames,
		Notes: notesText(a.EditorialNotes), Artwork: convArtwork(a.Artwork),
	}
	for _, al := range r.Views.FullAlbums.Data {
		out.Albums = append(out.Albums, convAlbum(al))
	}
	return out
}

func convPlaylist(r rawPlaylist) Playlist {
	a := r.Attributes
	desc := notesText(a.Description)
	if desc == "" {
		desc = notesText(a.EditorialNotes)
	}
	out := Playlist{
		ID: r.ID, Name: a.Name, CuratorName: a.CuratorName, Description: desc,
		PlaylistType: a.PlaylistType, LastModified: a.LastModifiedDate,
		Artwork: convArtwork(a.Artwork),
	}
	for _, t := range r.Relationships.Tracks.Data {
		if t.Type == "songs" {
			out.Tracks = append(out.Tracks, convSong(t))
		}
	}
	return out
}
