package catalog

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// The Apple Music *library* endpoints (`/v1/me/library/...`) are the signed-in
// account's own added music and playlists, as opposed to the public catalog.
// They authenticate with the Media-User-Token the client already sends on
// every request, and — unlike the catalog — are not storefront-scoped.
//
// Library resources have their own id space (`i.*` songs, `l.*` albums,
// `p.*` playlists) that the catalog stream/download pipeline does not
// understand. Every method here resolves each item to its **catalog**
// resource (via `include=catalog`, falling back to `playParams.catalogId`)
// so the ids handed back are the same ones `/v1/songs/{id}`,
// `/v1/albums/{id}` and the stream/download routes accept. Items with no
// catalog equivalent (uploaded or matched-only tracks) keep their library id
// and simply won't stream.

const (
	libraryPageLimit = 100
	libraryMaxPages  = 12 // ~1200 items — plenty for a library browse view
)

// LibraryPlaylist is one of the signed-in account's own playlists. CatalogID
// is Apple's catalog playlist id when the playlist mirrors a catalog one
// (`hasCatalog`); it is empty for a purely personal playlist, whose tracks
// still carry their individual catalog ids. Tracks is populated only by
// LibraryPlaylist(), not by LibraryPlaylists().
type LibraryPlaylist struct {
	ID          string   `json:"id"`
	CatalogID   string   `json:"catalogId,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	CanEdit     bool     `json:"canEdit"`
	TrackCount  int      `json:"trackCount,omitempty"`
	Artwork     *Artwork `json:"artwork,omitempty"`
	Tracks      []Song   `json:"tracks,omitempty"`
}

type rawLibraryPlayParams struct {
	ID        string `json:"id"`
	GlobalID  string `json:"globalId"`
	CatalogID string `json:"catalogId"`
}

type rawLibraryPlaylistAttrs struct {
	Name        string               `json:"name"`
	CanEdit     bool                 `json:"canEdit"`
	HasCatalog  bool                 `json:"hasCatalog"`
	Description *rawNotes            `json:"description"`
	Artwork     *rawArtwork          `json:"artwork"`
	PlayParams  rawLibraryPlayParams `json:"playParams"`
}

// rawLibraryResource is one item in a `/v1/me/library/...` list. With
// `include=catalog` Apple nests the matching catalog resource under
// relationships.catalog.data — that is the shape everything downstream wants,
// so it is decoded there in preference to the library attributes.
type rawLibraryResource struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Attributes    json.RawMessage `json:"attributes"`
	Relationships struct {
		Catalog struct {
			Data []json.RawMessage `json:"data"`
		} `json:"catalog"`
	} `json:"relationships"`
}

func libraryQuery() url.Values {
	return url.Values{
		"limit":   {strconv.Itoa(libraryPageLimit)},
		"include": {"catalog"},
	}
}

// LibraryPlaylists lists the account's library playlists (without tracks).
func (c *Client) LibraryPlaylists(ctx context.Context) ([]LibraryPlaylist, error) {
	out := []LibraryPlaylist{}
	path, q := "/v1/me/library/playlists", libraryQuery()
	for page := 0; page < libraryMaxPages && path != ""; page++ {
		var resp struct {
			Data []struct {
				ID         string                  `json:"id"`
				Attributes rawLibraryPlaylistAttrs `json:"attributes"`
			} `json:"data"`
			Next string `json:"next"`
		}
		if err := c.get(ctx, path, q, &resp); err != nil {
			if page == 0 {
				return nil, err
			}
			break // partial results beat failing the whole list
		}
		for _, d := range resp.Data {
			out = append(out, convLibraryPlaylist(d.ID, d.Attributes))
		}
		path, q = resp.Next, nil
	}
	return out, nil
}

// LibraryPlaylist returns one library playlist with its full track list, each
// track resolved to its catalog Song.
func (c *Client) LibraryPlaylist(ctx context.Context, id string) (*LibraryPlaylist, error) {
	var meta struct {
		Data []struct {
			ID         string                  `json:"id"`
			Attributes rawLibraryPlaylistAttrs `json:"attributes"`
		} `json:"data"`
	}
	metaPath := "/v1/me/library/playlists/" + url.PathEscape(id)
	if err := c.get(ctx, metaPath, url.Values{"include": {"catalog"}}, &meta); err != nil {
		return nil, err
	}
	if len(meta.Data) == 0 {
		return nil, ErrNotFound
	}
	pl := convLibraryPlaylist(meta.Data[0].ID, meta.Data[0].Attributes)

	path, q := metaPath+"/tracks", libraryQuery()
	for page := 0; page < libraryMaxPages && path != ""; page++ {
		var resp struct {
			Data []rawLibraryResource `json:"data"`
			Next string               `json:"next"`
		}
		if err := c.get(ctx, path, q, &resp); err != nil {
			break
		}
		for _, d := range resp.Data {
			if s, ok := librarySong(d); ok {
				pl.Tracks = append(pl.Tracks, s)
			}
		}
		path, q = resp.Next, nil
	}
	pl.TrackCount = len(pl.Tracks)
	return &pl, nil
}

// LibrarySongs lists the account's added songs, as catalog Songs.
func (c *Client) LibrarySongs(ctx context.Context) ([]Song, error) {
	out := []Song{}
	if err := c.libraryPages(ctx, "/v1/me/library/songs", func(d rawLibraryResource) {
		if s, ok := librarySong(d); ok {
			out = append(out, s)
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// LibraryAlbums lists the account's added albums, as catalog Albums (no tracks).
func (c *Client) LibraryAlbums(ctx context.Context) ([]Album, error) {
	out := []Album{}
	if err := c.libraryPages(ctx, "/v1/me/library/albums", func(d rawLibraryResource) {
		if a, ok := libraryAlbum(d); ok {
			out = append(out, a)
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// LibraryArtists lists the account's added artists, as catalog Artists (name
// and artwork only — no discography views).
func (c *Client) LibraryArtists(ctx context.Context) ([]Artist, error) {
	out := []Artist{}
	if err := c.libraryPages(ctx, "/v1/me/library/artists", func(d rawLibraryResource) {
		if a, ok := libraryArtist(d); ok {
			out = append(out, a)
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// libraryPages walks a `/v1/me/library/...` list, following `next`, and calls
// visit for every item. A mid-walk failure keeps whatever was collected so far.
func (c *Client) libraryPages(ctx context.Context, start string, visit func(rawLibraryResource)) error {
	path, q := start, libraryQuery()
	for page := 0; page < libraryMaxPages && path != ""; page++ {
		var resp struct {
			Data []rawLibraryResource `json:"data"`
			Next string               `json:"next"`
		}
		if err := c.get(ctx, path, q, &resp); err != nil {
			if page == 0 {
				return err
			}
			return nil
		}
		for _, d := range resp.Data {
			visit(d)
		}
		path, q = resp.Next, nil
	}
	return nil
}

func convLibraryPlaylist(id string, a rawLibraryPlaylistAttrs) LibraryPlaylist {
	pl := LibraryPlaylist{
		ID:          id,
		Name:        a.Name,
		Description: notesText(a.Description),
		CanEdit:     a.CanEdit,
		Artwork:     convArtwork(a.Artwork),
	}
	if a.HasCatalog {
		pl.CatalogID = a.PlayParams.GlobalID
	}
	return pl
}

// librarySong turns a library track into a catalog Song, preferring the
// nested catalog resource (include=catalog) and falling back to the library
// attributes keyed by playParams.catalogId.
func librarySong(d rawLibraryResource) (Song, bool) {
	if raw := firstCatalog(d); raw != nil {
		var rs rawSong
		if json.Unmarshal(raw, &rs) == nil && rs.ID != "" {
			return convSong(rs), true
		}
	}
	var la struct {
		rawSongAttrs
		PlayParams rawLibraryPlayParams `json:"playParams"`
	}
	if json.Unmarshal(d.Attributes, &la) != nil {
		return Song{}, false
	}
	id := la.PlayParams.CatalogID
	if id == "" {
		id = d.ID
	}
	return Song{
		ID: id, Name: la.Name, ArtistName: la.ArtistName,
		AlbumName: la.AlbumName, DiscNumber: la.DiscNumber, TrackNumber: la.TrackNumber,
		DurationMs: la.DurationInMillis, ReleaseDate: la.ReleaseDate,
		Genres: la.GenreNames, ContentRating: la.ContentRating,
		HasLyrics: la.HasLyrics, HasSyncLyrics: la.HasTimeSyncedLyrics,
		Quality: convQuality(la.AudioTraits), Artwork: convArtwork(la.Artwork),
	}, true
}

func libraryAlbum(d rawLibraryResource) (Album, bool) {
	if raw := firstCatalog(d); raw != nil {
		var ra rawAlbum
		if json.Unmarshal(raw, &ra) == nil && ra.ID != "" {
			return convAlbum(ra), true
		}
	}
	var la struct {
		rawAlbumAttrs
		PlayParams rawLibraryPlayParams `json:"playParams"`
	}
	if json.Unmarshal(d.Attributes, &la) != nil {
		return Album{}, false
	}
	id := la.PlayParams.CatalogID
	if id == "" {
		id = d.ID
	}
	return Album{
		ID: id, Name: la.Name, ArtistName: la.ArtistName,
		TrackCount: la.TrackCount, ReleaseDate: la.ReleaseDate,
		Genres: la.GenreNames, ContentRating: la.ContentRating,
		IsSingle: la.IsSingle, IsCompilation: la.IsCompilation,
		Quality: convQuality(la.AudioTraits), Artwork: convArtwork(la.Artwork),
	}, true
}

func libraryArtist(d rawLibraryResource) (Artist, bool) {
	if raw := firstCatalog(d); raw != nil {
		var ra rawArtist
		if json.Unmarshal(raw, &ra) == nil && ra.ID != "" {
			a := convArtist(ra)
			return Artist{ID: a.ID, Name: a.Name, Genres: a.Genres, Artwork: a.Artwork}, true
		}
	}
	var la struct {
		Name    string      `json:"name"`
		Artwork *rawArtwork `json:"artwork"`
	}
	if json.Unmarshal(d.Attributes, &la) != nil || la.Name == "" {
		return Artist{}, false
	}
	return Artist{ID: d.ID, Name: la.Name, Artwork: convArtwork(la.Artwork)}, true
}

func firstCatalog(d rawLibraryResource) json.RawMessage {
	if len(d.Relationships.Catalog.Data) > 0 {
		return d.Relationships.Catalog.Data[0]
	}
	return nil
}
