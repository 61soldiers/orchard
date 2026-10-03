package catalog

import (
	"encoding/json"
	"testing"
)

// The library-write payloads are the one part of these endpoints that can go
// wrong silently: Apple answers a malformed relationship with a 400 that says
// nothing useful, and a wrong resource type puts the right song into the
// wrong playlist slot. These cover the shape of what goes on the wire.

func TestTrackRefTypeDistinguishesCatalogFromLibraryIDs(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"1440857781", "songs"},
		{"i.4YQ8bYAtNGpBoZ", "library-songs"},
		{"pl.u-abc", "songs"},
	}
	for _, tc := range cases {
		if got := trackRefType(tc.id); got != tc.want {
			t.Errorf("trackRefType(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestTrackRefsSkipsBlanksAndKeepsOrder(t *testing.T) {
	refs := trackRefs([]string{"1", "", "  ", "i.abc", " 3 "})
	if len(refs) != 3 {
		t.Fatalf("len(refs) = %d, want 3 (blanks dropped)", len(refs))
	}
	if refs[0].ID != "1" || refs[0].Type != "songs" {
		t.Errorf("refs[0] = %+v", refs[0])
	}
	if refs[1].ID != "i.abc" || refs[1].Type != "library-songs" {
		t.Errorf("refs[1] = %+v", refs[1])
	}
	// Order is the playlist's order — a reorder that shuffled ids here would
	// be undetectable from the caller's side.
	if refs[2].ID != "3" {
		t.Errorf("refs[2].ID = %q, want %q (trimmed, order preserved)", refs[2].ID, "3")
	}
}

func TestCreatePlaylistPayloadOmitsEmptyRelationship(t *testing.T) {
	encoded, err := json.Marshal(newCreatePlaylistPayload("Road Trip", "", nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["relationships"]; ok {
		// Apple rejects an empty tracks relationship rather than treating it
		// as "no tracks", so an empty playlist must send no relationship.
		t.Errorf("empty track list should omit relationships, got %s", encoded)
	}
	attrs, _ := decoded["attributes"].(map[string]any)
	if attrs["name"] != "Road Trip" {
		t.Errorf("attributes.name = %v, want Road Trip", attrs["name"])
	}
	if _, ok := attrs["description"]; ok {
		t.Errorf("empty description should be omitted, got %s", encoded)
	}
}

func TestCreatePlaylistPayloadCarriesTracks(t *testing.T) {
	encoded, err := json.Marshal(newCreatePlaylistPayload("Mix", "notes", []string{"1", "2"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Attributes struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"attributes"`
		Relationships struct {
			Tracks struct {
				Data []libraryResourceRef `json:"data"`
			} `json:"tracks"`
		} `json:"relationships"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Attributes.Description != "notes" {
		t.Errorf("description = %q", decoded.Attributes.Description)
	}
	if len(decoded.Relationships.Tracks.Data) != 2 {
		t.Fatalf("tracks = %+v, want 2", decoded.Relationships.Tracks.Data)
	}
	if decoded.Relationships.Tracks.Data[0].Type != "songs" {
		t.Errorf("track type = %q, want songs", decoded.Relationships.Tracks.Data[0].Type)
	}
}

func TestWritesValidateBeforeMakingARequest(t *testing.T) {
	c := &Client{}
	//nolint:staticcheck // nil ctx is never reached: every call below fails validation first.
	if _, err := c.CreateLibraryPlaylist(nil, "   ", "", nil); err == nil {
		t.Error("CreateLibraryPlaylist with a blank name should fail before any request")
	}
	//nolint:staticcheck // see above
	if err := c.AddLibraryPlaylistTracks(nil, "p.abc", nil); err != ErrEmptyTrackList {
		t.Errorf("AddLibraryPlaylistTracks with no tracks: err = %v, want ErrEmptyTrackList", err)
	}
	//nolint:staticcheck // see above
	if err := c.SetLibraryPlaylistTracks(nil, "p.abc", nil, false); err != ErrEmptyTrackList {
		t.Errorf("SetLibraryPlaylistTracks with no tracks: err = %v, want ErrEmptyTrackList", err)
	}
	//nolint:staticcheck // see above
	if err := c.UpdateLibraryPlaylist(nil, "p.abc", "", ""); err == nil {
		t.Error("UpdateLibraryPlaylist with nothing to change should fail before any request")
	}
	//nolint:staticcheck // see above
	if err := c.DeleteLibraryPlaylist(nil, ""); err == nil {
		t.Error("DeleteLibraryPlaylist with no id should fail before any request")
	}
}
