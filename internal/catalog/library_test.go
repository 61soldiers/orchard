package catalog

import (
	"encoding/json"
	"testing"
)

func rawResource(t *testing.T, jsonBody string) rawLibraryResource {
	t.Helper()
	var d rawLibraryResource
	if err := json.Unmarshal([]byte(jsonBody), &d); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return d
}

func TestLibrarySongPrefersCatalogResource(t *testing.T) {
	d := rawResource(t, `{
		"id": "i.abc",
		"type": "library-songs",
		"attributes": {"name": "Library Name", "artistName": "Library Artist"},
		"relationships": {
			"catalog": {"data": [{
				"id": "123456",
				"type": "songs",
				"attributes": {"name": "Catalog Name", "artistName": "Catalog Artist"}
			}]}
		}
	}`)

	got, ok := librarySong(d)
	if !ok {
		t.Fatal("librarySong() ok = false")
	}
	if got.ID != "123456" || got.Name != "Catalog Name" || got.ArtistName != "Catalog Artist" {
		t.Errorf("librarySong() = %+v, want the catalog-resolved song", got)
	}
}

func TestLibrarySongFallsBackToLibraryAttrsWithoutCatalogMatch(t *testing.T) {
	d := rawResource(t, `{
		"id": "i.abc",
		"type": "library-songs",
		"attributes": {
			"name": "Uploaded Track", "artistName": "Some Artist",
			"playParams": {"id": "i.abc"}
		}
	}`)

	got, ok := librarySong(d)
	if !ok {
		t.Fatal("librarySong() ok = false")
	}
	// No catalogId in playParams and no catalog relationship: falls back to
	// the library id itself, since that's all there is to identify it by.
	if got.ID != "i.abc" || got.Name != "Uploaded Track" {
		t.Errorf("librarySong() = %+v, want library-id fallback", got)
	}
}

func TestLibrarySongFallsBackToCatalogIDFromPlayParams(t *testing.T) {
	d := rawResource(t, `{
		"id": "i.abc",
		"type": "library-songs",
		"attributes": {
			"name": "Matched Track", "artistName": "Some Artist",
			"playParams": {"id": "i.abc", "catalogId": "999"}
		}
	}`)

	got, ok := librarySong(d)
	if !ok {
		t.Fatal("librarySong() ok = false")
	}
	if got.ID != "999" {
		t.Errorf("librarySong().ID = %q, want catalogId fallback %q", got.ID, "999")
	}
}

func TestLibraryAlbumAndArtistCatalogPreference(t *testing.T) {
	album := rawResource(t, `{
		"id": "l.abc", "type": "library-albums",
		"attributes": {"name": "Library Album", "artistName": "X"},
		"relationships": {"catalog": {"data": [{
			"id": "42", "type": "albums",
			"attributes": {"name": "Catalog Album", "artistName": "X"}
		}]}}
	}`)
	if a, ok := libraryAlbum(album); !ok || a.ID != "42" || a.Name != "Catalog Album" {
		t.Errorf("libraryAlbum() = %+v, %v, want catalog-resolved album with id 42", a, ok)
	}

	artist := rawResource(t, `{
		"id": "r.abc", "type": "library-artists",
		"attributes": {"name": "Library Artist"},
		"relationships": {"catalog": {"data": [{
			"id": "77", "type": "artists",
			"attributes": {"name": "Catalog Artist"}
		}]}}
	}`)
	if a, ok := libraryArtist(artist); !ok || a.ID != "77" || a.Name != "Catalog Artist" {
		t.Errorf("libraryArtist() = %+v, %v, want catalog-resolved artist with id 77", a, ok)
	}
}

func TestLibraryArtistRequiresAName(t *testing.T) {
	d := rawResource(t, `{"id": "r.abc", "type": "library-artists", "attributes": {}}`)
	if _, ok := libraryArtist(d); ok {
		t.Error("libraryArtist() ok = true for an attrs blob with no name and no catalog match")
	}
}

func TestConvLibraryPlaylistCatalogID(t *testing.T) {
	withCatalog := convLibraryPlaylist("p.1", rawLibraryPlaylistAttrs{
		Name: "Mirrors a catalog playlist", HasCatalog: true,
		PlayParams: rawLibraryPlayParams{GlobalID: "pl.u-xyz"},
	})
	if withCatalog.CatalogID != "pl.u-xyz" {
		t.Errorf("CatalogID = %q, want pl.u-xyz", withCatalog.CatalogID)
	}

	personal := convLibraryPlaylist("p.2", rawLibraryPlaylistAttrs{
		Name: "Purely personal", HasCatalog: false,
		PlayParams: rawLibraryPlayParams{GlobalID: "pl.u-should-be-ignored"},
	})
	if personal.CatalogID != "" {
		t.Errorf("CatalogID = %q, want empty for a playlist with no catalog mirror", personal.CatalogID)
	}
}

func TestLibraryPageRejectsInvalidCursor(t *testing.T) {
	c := &Client{}
	_, err := c.libraryPage(nil, "/v1/me/library/songs", "https://evil.example.com/steal", func(rawLibraryResource) {}) //nolint:staticcheck // nil ctx never reached: cursor is validated first
	if err != ErrInvalidCursor {
		t.Errorf("libraryPage with an absolute-URL cursor: err = %v, want ErrInvalidCursor", err)
	}
}

func TestLibrarySongsRejectsInvalidCursor(t *testing.T) {
	c := &Client{}
	_, _, err := c.LibrarySongs(nil, "not-a-relative-path") //nolint:staticcheck // nil ctx never reached: cursor is validated first
	if err != ErrInvalidCursor {
		t.Errorf("LibrarySongs with a bad cursor: err = %v, want ErrInvalidCursor", err)
	}
}

func TestLibraryQueryOptions(t *testing.T) {
	q := libraryQuery()
	if q.Get("limit") != "100" || q.Get("sort") != "" {
		t.Fatalf("default query = %v, want limit=100 and no sort", q)
	}
	q = libraryQuery(WithLibraryLimit(24), WithLibraryRecent())
	if q.Get("limit") != "24" || q.Get("sort") != "-dateAdded" {
		t.Fatalf("tuned query = %v, want limit=24 sort=-dateAdded", q)
	}
	// Out-of-range limits fall back to the default rather than reaching Apple.
	for _, n := range []int{0, -3, 101} {
		if got := libraryQuery(WithLibraryLimit(n)).Get("limit"); got != "100" {
			t.Errorf("limit %d produced %q, want 100", n, got)
		}
	}
}

func TestWithQueryParam(t *testing.T) {
	got := withQueryParam("/v1/me/library/albums?offset=12&sort=-dateAdded", "limit", "12")
	want := "/v1/me/library/albums?limit=12&offset=12&sort=-dateAdded"
	if got != want {
		t.Fatalf("withQueryParam = %q, want %q", got, want)
	}
	if got := withQueryParam("/v1/me/library/albums", "limit", "5"); got != "/v1/me/library/albums?limit=5" {
		t.Fatalf("no-query path = %q", got)
	}
}
