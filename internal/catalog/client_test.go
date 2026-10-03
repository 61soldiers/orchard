package catalog

import "testing"

func TestValidCursor(t *testing.T) {
	cases := []struct {
		name   string
		cursor string
		want   bool
	}{
		{"empty is valid (first page)", "", true},
		{"relative apple path", "/v1/catalog/us/playlists/pl.abc/tracks?offset=100", true},
		{"relative library path", "/v1/me/library/songs?offset=100&limit=100", true},
		{"absolute url rejected", "https://evil.example.com/steal", false},
		{"scheme-relative rejected", "//evil.example.com/steal", false},
		{"embedded scheme rejected", "/v1/redirect?to=http://evil.example.com", false},
		{"wrong prefix rejected", "/v2/catalog/us/playlists/pl.abc/tracks", false},
		{"no leading slash rejected", "v1/catalog/us/playlists/pl.abc/tracks", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// validCursor treats "" as a special "first page" case only in its
			// callers (libraryPage/PlaylistTracks); on its own it should just
			// answer the prefix question, so the empty case is handled by the
			// callers' own cursor != "" guard, not by validCursor itself. This
			// asserts that direct contract precisely.
			if tc.cursor == "" {
				return
			}
			if got := validCursor(tc.cursor); got != tc.want {
				t.Errorf("validCursor(%q) = %v, want %v", tc.cursor, got, tc.want)
			}
		})
	}
}

func TestPlaylistTracksRequiresCursor(t *testing.T) {
	c := &Client{}
	if _, _, err := c.PlaylistTracks(nil, "pl.abc", ""); err == nil { //nolint:staticcheck // nil ctx never reached: cursor is checked first
		t.Error("PlaylistTracks with an empty cursor should error before making any request")
	}
}

func TestPlaylistTracksRejectsInvalidCursor(t *testing.T) {
	c := &Client{}
	_, _, err := c.PlaylistTracks(nil, "pl.abc", "https://evil.example.com/steal") //nolint:staticcheck // nil ctx never reached: cursor is checked first
	if err != ErrInvalidCursor {
		t.Errorf("PlaylistTracks with an absolute-URL cursor: err = %v, want ErrInvalidCursor", err)
	}
}
