package catalog

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// Pin is one item the user pinned to the top of their Apple Music library.
//
// Pins are a Library feature of Apple's own apps (long-press → Pin), stored
// server-side and synced across the account's devices. They are not part of
// MusicKit's surface at all — `/v1/me/library/pins` was found by probing a
// live account — so treat the shape here as observed rather than documented.
type Pin struct {
	// Type is normalised to the catalog spelling ("playlists", "albums",
	// "artists", "songs"), with Apple's "library-" prefix stripped, so a
	// client can route on it the same way it routes any other item.
	Type string `json:"type"`

	// ID is the id a client should open: the catalog id whenever the pin
	// resolves to one, otherwise the library id.
	ID string `json:"id"`

	// CatalogID is set when the pinned item exists in Apple's catalog — which
	// is what makes it streamable. LibraryID is set when the pin is a library
	// resource. A purely personal playlist has a LibraryID and no CatalogID.
	CatalogID string `json:"catalogId,omitempty"`
	LibraryID string `json:"libraryId,omitempty"`

	Name        string   `json:"name"`
	ArtistName  string   `json:"artistName,omitempty"`
	CuratorName string   `json:"curatorName,omitempty"`
	Artwork     *Artwork `json:"artwork,omitempty"`
}

// rawPinAttrs covers every pinnable type with one struct: Apple's library and
// catalog resources agree on these field names, and the ones that don't apply
// are simply absent.
type rawPinAttrs struct {
	Name        string               `json:"name"`
	ArtistName  string               `json:"artistName"`
	CuratorName string               `json:"curatorName"`
	Artwork     *rawArtwork          `json:"artwork"`
	PlayParams  rawLibraryPlayParams `json:"playParams"`
}

// Pins returns the account's pinned library items, in Apple's own order.
//
// `include=catalog` is requested for the same reason every other library read
// does it: a library id (`p.*`, `l.*`, `i.*`) is useless to the stream and
// download pipeline, so each pin is resolved to its catalog resource before it
// leaves. The catalog resource is also preferred for name and artwork — a
// library playlist's artwork is an auto-generated mosaic where the catalog one
// is the real cover.
func (c *Client) Pins(ctx context.Context) ([]Pin, error) {
	var out struct {
		Data []rawLibraryResource `json:"data"`
	}
	q := url.Values{"include": {"catalog"}}
	if err := c.get(ctx, "/v1/me/library/pins", q, &out); err != nil {
		return nil, err
	}

	pins := make([]Pin, 0, len(out.Data))
	for _, r := range out.Data {
		pin := Pin{Type: strings.TrimPrefix(r.Type, "library-")}
		if strings.HasPrefix(r.Type, "library-") {
			pin.LibraryID = r.ID
		}

		var libAttrs rawPinAttrs
		if len(r.Attributes) > 0 {
			_ = json.Unmarshal(r.Attributes, &libAttrs)
		}
		applyPinAttrs(&pin, libAttrs)

		// The nested catalog resource, when Apple included one, is the better
		// source for everything it carries — and the only source of a usable id.
		if len(r.Relationships.Catalog.Data) > 0 {
			var cat struct {
				ID         string      `json:"id"`
				Type       string      `json:"type"`
				Attributes rawPinAttrs `json:"attributes"`
			}
			if err := json.Unmarshal(r.Relationships.Catalog.Data[0], &cat); err == nil {
				pin.CatalogID = cat.ID
				if cat.Type != "" {
					pin.Type = cat.Type
				}
				applyPinAttrs(&pin, cat.Attributes)
			}
		}

		// Fall back to the ids Apple puts in playParams when the catalog
		// relationship is missing. A library playlist names it globalId; songs
		// and albums use catalogId.
		if pin.CatalogID == "" {
			if id := libAttrs.PlayParams.GlobalID; id != "" {
				pin.CatalogID = id
			} else if id := libAttrs.PlayParams.CatalogID; id != "" {
				pin.CatalogID = id
			}
		}

		pin.ID = pin.CatalogID
		if pin.ID == "" {
			pin.ID = r.ID
		}
		if pin.ID == "" || pin.Name == "" {
			continue
		}
		pins = append(pins, pin)
	}
	return pins, nil
}

// applyPinAttrs copies whatever a source actually carries, leaving already-set
// fields alone when the newer source is silent — so the catalog resource can
// enrich a pin without blanking what the library resource supplied.
func applyPinAttrs(pin *Pin, a rawPinAttrs) {
	if a.Name != "" {
		pin.Name = a.Name
	}
	if a.ArtistName != "" {
		pin.ArtistName = a.ArtistName
	}
	if a.CuratorName != "" {
		pin.CuratorName = a.CuratorName
	}
	if art := convArtwork(a.Artwork); art != nil {
		pin.Artwork = art
	}
}
