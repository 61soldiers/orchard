package catalog

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Writing to the signed-in account's own library — creating a playlist,
// renaming one, adding tracks to one, reordering it — is the second thing in
// Orchard that *changes* something on Apple's side (the first being
// internal/playactivity). Everything else in this package reads.
//
// Apple models a library playlist's contents as a JSON:API relationship, so
// the four operations map onto it directly:
//
//	POST   /v1/me/library/playlists             create, optionally with tracks
//	PATCH  /v1/me/library/playlists/{id}        rename / re-describe
//	POST   /v1/me/library/playlists/{id}/tracks append tracks
//	PUT    /v1/me/library/playlists/{id}/tracks replace the whole ordered list
//	DELETE /v1/me/library/playlists/{id}        remove the playlist
//
// Reordering and removing a single track are both the PUT: Apple exposes no
// "move track" operation, so the client sends the full list in its new order
// and Apple replaces what it has. That is why SetLibraryPlaylistTracks takes
// every id rather than a delta — a partial list would silently truncate the
// playlist.
//
// Only a playlist the account actually owns can be written to: Apple answers
// a subscription (catalog-mirrored) playlist with 403, which surfaces as an
// APIError the API layer maps to a plain "this playlist can't be edited".

// ErrEmptyTrackList guards the one mistake that would quietly destroy a
// playlist: replacing its contents with nothing. A caller that really means
// "empty this playlist" has to say so explicitly (AllowEmpty).
var ErrEmptyTrackList = errors.New("track list is empty")

// libraryResourceRef is one entry in a playlist's tracks relationship.
type libraryResourceRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// trackRefType picks the JSON:API type for one track id.
//
// Every list this package hands out resolves a library track to its *catalog*
// song where one exists (see librarySong), so ids arriving back here are
// normally catalog ids and take type "songs". A track with no catalog
// equivalent — uploaded, or matched-only — keeps its library id, which Apple
// spells with an "i." prefix, and must go back as "library-songs" or the
// write is rejected.
func trackRefType(id string) string {
	if strings.HasPrefix(id, "i.") {
		return "library-songs"
	}
	return "songs"
}

// trackRefs maps track ids onto Apple's relationship shape, skipping blanks
// so one empty id in a list can't fail the whole write.
func trackRefs(ids []string) []libraryResourceRef {
	refs := make([]libraryResourceRef, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		refs = append(refs, libraryResourceRef{ID: id, Type: trackRefType(id)})
	}
	return refs
}

// createPlaylistPayload is the body Apple expects for a new library playlist.
type createPlaylistPayload struct {
	Attributes struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	} `json:"attributes"`
	Relationships *struct {
		Tracks struct {
			Data []libraryResourceRef `json:"data"`
		} `json:"tracks"`
	} `json:"relationships,omitempty"`
}

func newCreatePlaylistPayload(name, description string, trackIDs []string) createPlaylistPayload {
	var p createPlaylistPayload
	p.Attributes.Name = name
	p.Attributes.Description = description
	if refs := trackRefs(trackIDs); len(refs) > 0 {
		p.Relationships = &struct {
			Tracks struct {
				Data []libraryResourceRef `json:"data"`
			} `json:"tracks"`
		}{}
		p.Relationships.Tracks.Data = refs
	}
	return p
}

// updatePlaylistPayload renames / re-describes an existing playlist. Apple
// wraps a PATCH in a resource envelope, unlike the create above.
type updatePlaylistPayload struct {
	Attributes struct {
		Name        string `json:"name,omitempty"`
		Description string `json:"description,omitempty"`
	} `json:"attributes"`
}

// CreateLibraryPlaylist creates a playlist in the account's library and
// returns it. trackIDs may be empty, for an empty playlist.
func (c *Client) CreateLibraryPlaylist(ctx context.Context, name, description string, trackIDs []string) (*LibraryPlaylist, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("playlist name is required")
	}

	var resp struct {
		Data []struct {
			ID         string                  `json:"id"`
			Attributes rawLibraryPlaylistAttrs `json:"attributes"`
		} `json:"data"`
	}
	body := newCreatePlaylistPayload(name, description, trackIDs)
	// Not retried: a create is the one write here that isn't idempotent, and
	// a retried timeout would leave the user with two identical playlists.
	if err := c.sendJSON(ctx, http.MethodPost, "/v1/me/library/playlists", body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		// Apple occasionally answers a successful create with an empty body.
		// The playlist exists; the caller just has to re-list to see it.
		return &LibraryPlaylist{Name: name, Description: description, CanEdit: true}, nil
	}
	pl := convLibraryPlaylist(resp.Data[0].ID, resp.Data[0].Attributes)
	return &pl, nil
}

// UpdateLibraryPlaylist renames and/or re-describes a playlist. An empty
// string leaves that attribute as it is.
func (c *Client) UpdateLibraryPlaylist(ctx context.Context, id, name, description string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("playlist id is required")
	}
	if strings.TrimSpace(name) == "" && strings.TrimSpace(description) == "" {
		return errors.New("nothing to update")
	}
	var body updatePlaylistPayload
	body.Attributes.Name = strings.TrimSpace(name)
	body.Attributes.Description = strings.TrimSpace(description)
	return c.sendIdempotent(ctx, http.MethodPatch, "/v1/me/library/playlists/"+url.PathEscape(id), body)
}

// AddLibraryPlaylistTracks appends tracks to the end of a playlist.
func (c *Client) AddLibraryPlaylistTracks(ctx context.Context, id string, trackIDs []string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("playlist id is required")
	}
	refs := trackRefs(trackIDs)
	if len(refs) == 0 {
		return ErrEmptyTrackList
	}
	body := struct {
		Data []libraryResourceRef `json:"data"`
	}{Data: refs}
	// Appending the same track twice is what Apple's own clients do when a
	// user taps twice, so a retried POST is at worst a visible duplicate
	// rather than a corrupted playlist — but it is still not idempotent, so
	// this doesn't retry either.
	return c.sendJSON(ctx, http.MethodPost, "/v1/me/library/playlists/"+url.PathEscape(id)+"/tracks", body, nil)
}

// SetLibraryPlaylistTracks replaces a playlist's contents with exactly
// trackIDs, in that order — how both reordering and removing a track are
// done, since Apple offers neither as its own operation.
//
// allowEmpty has to be set to clear a playlist: without it an empty list is
// refused rather than silently wiping the playlist, which is the shape a bug
// upstream (a failed fetch, an unresolved page) would take.
func (c *Client) SetLibraryPlaylistTracks(ctx context.Context, id string, trackIDs []string, allowEmpty bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("playlist id is required")
	}
	refs := trackRefs(trackIDs)
	if len(refs) == 0 && !allowEmpty {
		return ErrEmptyTrackList
	}
	body := struct {
		Data []libraryResourceRef `json:"data"`
	}{Data: refs}
	return c.sendIdempotent(ctx, http.MethodPut, "/v1/me/library/playlists/"+url.PathEscape(id)+"/tracks", body)
}

// DeleteLibraryPlaylist removes a playlist from the account's library.
func (c *Client) DeleteLibraryPlaylist(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("playlist id is required")
	}
	return c.sendIdempotent(ctx, http.MethodDelete, "/v1/me/library/playlists/"+url.PathEscape(id), nil)
}
