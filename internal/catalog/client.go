// Package catalog is a client for Apple's public music catalog API.
//
// It authenticates with the developer and media-user tokens the wrapper daemon
// already holds, so nothing here has to scrape the web player. Responses are
// normalised into this package's own types rather than passed through, so the
// HTTP surface stays stable if Apple reshuffles theirs.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchard/internal/ratelimit"
	"orchard/internal/retry"
)

const (
	ampBase = "https://amp-api.music.apple.com"
	// origin is required; Apple rejects catalog calls without it.
	origin = "https://music.apple.com"

	storefrontTTL = time.Hour
	maxConcurrent = 8
)

var (
	// ErrNotFound is returned for a 404 from Apple.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized means the tokens were rejected; the session likely expired.
	ErrUnauthorized = errors.New("apple rejected the session tokens")
)

// TokenSource supplies the developer and media-user tokens.
type TokenSource func(ctx context.Context) (devToken, musicToken string, err error)

// Config tunes how the client paces itself against Apple.
type Config struct {
	// RateLimit is the steady-state requests-per-second budget for outbound
	// calls; zero or negative disables limiting. RateBurst is how many
	// requests may go out back-to-back before that budget applies.
	RateLimit float64
	RateBurst int
}

// Client talks to the Apple Music catalog.
type Client struct {
	tokens  TokenSource
	hc      *http.Client
	sem     chan struct{}
	limiter *ratelimit.Limiter

	mu           sync.Mutex
	storefront   string
	storefrontAt time.Time
}

// New returns a Client that authenticates using tokens.
func New(tokens TokenSource, cfg Config) *Client {
	return &Client{
		tokens:  tokens,
		hc:      &http.Client{Timeout: 30 * time.Second},
		sem:     make(chan struct{}, maxConcurrent),
		limiter: ratelimit.New(cfg.RateLimit, cfg.RateBurst),
	}
}

// APIError carries Apple's own error detail.
type APIError struct {
	Status int
	Title  string
	Detail string

	// retryAfter is set from a 429's Retry-After header, in seconds form.
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("apple catalog: %s (%d)", e.Detail, e.Status)
	}
	return fmt.Sprintf("apple catalog: %s (%d)", e.Title, e.Status)
}

// RetryAfter implements retry.Afterer so a 429 with an explicit wait is
// honoured instead of guessed at.
func (e *APIError) RetryAfter() (time.Duration, bool) {
	if e.retryAfter > 0 {
		return e.retryAfter, true
	}
	return 0, false
}

// isRetryableErr decides whether get should try again. Not-found and
// unauthorized are permanent for a given request; everything else that is
// not a well-formed 4xx from Apple is treated as transient.
func isRetryableErr(err error) bool {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnauthorized) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// A transport-level failure (dial, TLS, timeout) or a truncated response
	// body: exactly the transient case retries exist for.
	return true
}

// Storefront resolves the signed-in account's storefront code, e.g. "us".
func (c *Client) Storefront(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.storefront != "" && time.Since(c.storefrontAt) < storefrontTTL {
		sf := c.storefront
		c.mu.Unlock()
		return sf, nil
	}
	c.mu.Unlock()

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/v1/me/storefront", nil, &out); err != nil {
		return "", err
	}
	if len(out.Data) == 0 || out.Data[0].ID == "" {
		return "", errors.New("apple returned no storefront for this account")
	}

	c.mu.Lock()
	c.storefront, c.storefrontAt = out.Data[0].ID, time.Now()
	c.mu.Unlock()
	return out.Data[0].ID, nil
}

// SearchTypes are the catalog types Search understands.
var SearchTypes = []string{"songs", "albums", "artists", "playlists"}

// Search queries the catalog. types defaults to all of SearchTypes; limit is
// clamped to Apple's maximum of 25.
func (c *Client) Search(ctx context.Context, term string, types []string, limit int) (*SearchResults, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return nil, errors.New("term is required")
	}
	if len(types) == 0 {
		types = SearchTypes
	}
	for _, t := range types {
		if !validType(t) {
			return nil, fmt.Errorf("unknown search type %q", t)
		}
	}
	if limit <= 0 {
		limit = 25
	}
	if limit > 25 {
		limit = 25
	}

	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("term", term)
	q.Set("types", strings.Join(types, ","))
	q.Set("limit", strconv.Itoa(limit))

	var raw rawSearchResponse
	if err := c.get(ctx, "/v1/catalog/"+sf+"/search", q, &raw); err != nil {
		return nil, err
	}

	out := &SearchResults{}
	for _, s := range raw.Results.Songs.Data {
		out.Songs = append(out.Songs, convSong(s))
	}
	for _, a := range raw.Results.Albums.Data {
		out.Albums = append(out.Albums, convAlbum(a))
	}
	for _, a := range raw.Results.Artists.Data {
		out.Artists = append(out.Artists, convArtist(a))
	}
	for _, p := range raw.Results.Playlists.Data {
		out.Playlists = append(out.Playlists, convPlaylist(p))
	}
	return out, nil
}

func validType(t string) bool {
	for _, v := range SearchTypes {
		if v == t {
			return true
		}
	}
	return false
}

// Album returns an album with its full track list.
func (c *Client) Album(ctx context.Context, id string) (*Album, error) {
	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"include": {"tracks"}}

	var out struct {
		Data []rawAlbum `json:"data"`
	}
	if err := c.get(ctx, "/v1/catalog/"+sf+"/albums/"+url.PathEscape(id), q, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, ErrNotFound
	}
	a := convAlbum(out.Data[0])
	return &a, nil
}

// Artist returns an artist with their albums.
func (c *Client) Artist(ctx context.Context, id string) (*Artist, error) {
	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"views": {"full-albums"}}

	var out struct {
		Data []rawArtist `json:"data"`
	}
	if err := c.get(ctx, "/v1/catalog/"+sf+"/artists/"+url.PathEscape(id), q, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, ErrNotFound
	}
	a := convArtist(out.Data[0])
	return &a, nil
}

// Playlist returns a playlist with its tracks. Apple pages long playlists, so
// this follows the tracks relationship until it is exhausted.
func (c *Client) Playlist(ctx context.Context, id string) (*Playlist, error) {
	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"include": {"tracks"}}

	var out struct {
		Data []rawPlaylist `json:"data"`
	}
	if err := c.get(ctx, "/v1/catalog/"+sf+"/playlists/"+url.PathEscape(id), q, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, ErrNotFound
	}
	p := convPlaylist(out.Data[0])

	next := out.Data[0].Relationships.Tracks.Next
	for next != "" {
		var page struct {
			Data []rawSong `json:"data"`
			Next string    `json:"next"`
		}
		if err := c.get(ctx, next, nil, &page); err != nil {
			// Partial results beat failing the whole request.
			break
		}
		for _, t := range page.Data {
			if t.Type == "songs" {
				p.Tracks = append(p.Tracks, convSong(t))
			}
		}
		if page.Next == next {
			break
		}
		next = page.Next
	}
	return &p, nil
}

// Song returns a single track.
func (c *Client) Song(ctx context.Context, id string) (*Song, error) {
	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []rawSong `json:"data"`
	}
	if err := c.get(ctx, "/v1/catalog/"+sf+"/songs/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, ErrNotFound
	}
	s := convSong(out.Data[0])
	return &s, nil
}

// Lyrics returns the TTML lyrics document for a song. Not every track has one,
// and it requires an active subscription.
func (c *Client) Lyrics(ctx context.Context, id string) (*Lyrics, error) {
	sf, err := c.Storefront(ctx)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				TTML string `json:"ttml"`
			} `json:"attributes"`
		} `json:"data"`
	}
	path := "/v1/catalog/" + sf + "/songs/" + url.PathEscape(id) + "/lyrics"
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 || out.Data[0].Attributes.TTML == "" {
		return nil, ErrNotFound
	}

	lyrics := &Lyrics{SongID: id, Format: "ttml", TTML: out.Data[0].Attributes.TTML}
	// Best-effort: a track whose TTML doesn't convert cleanly still has its
	// raw TTML returned, just without LRC/SyncLevel.
	if lrc, level, err := ttmlToLRC(lyrics.TTML); err == nil {
		lyrics.LRC, lyrics.SyncLevel = lrc, level
	}
	return lyrics, nil
}

// Recommendations returns Apple's "Made For You" personalization: named
// personal playlists ("New Music", "Get Up!", "Chill" and so on) bundled a
// few to a group, plus themed collections and station groups. A caller
// looking for one mix by name should search Items across every group, not
// group titles — see RecommendationGroup.
func (c *Client) Recommendations(ctx context.Context) ([]RecommendationGroup, error) {
	var out struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Title  rawDisplayString `json:"title"`
				Reason rawDisplayString `json:"reason"`
			} `json:"attributes"`
			Relationships struct {
				Contents struct {
					Data []rawMixedResource `json:"data"`
				} `json:"contents"`
			} `json:"relationships"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/v1/me/recommendations", nil, &out); err != nil {
		return nil, err
	}

	groups := make([]RecommendationGroup, 0, len(out.Data))
	for _, r := range out.Data {
		g := RecommendationGroup{
			ID:     r.ID,
			Title:  r.Attributes.Title.StringForDisplay,
			Reason: r.Attributes.Reason.StringForDisplay,
		}
		for _, res := range r.Relationships.Contents.Data {
			if it, ok := toItem(res); ok {
				g.Items = append(g.Items, it)
			}
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// RecentlyPlayed lists what the signed-in account played most recently —
// songs, albums, playlists or stations, in whatever mix Apple returns.
func (c *Client) RecentlyPlayed(ctx context.Context, limit int) ([]Item, error) {
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}

	var out struct {
		Data []rawMixedResource `json:"data"`
	}
	if err := c.get(ctx, "/v1/me/recent/played", q, &out); err != nil {
		return nil, err
	}

	items := make([]Item, 0, len(out.Data))
	for _, r := range out.Data {
		if it, ok := toItem(r); ok {
			items = append(items, it)
		}
	}
	return items, nil
}

// get performs an authenticated GET and decodes the response into dst, retrying
// transient failures with backoff. path may be an absolute Apple path or a
// full URL from a "next" link.
//
// Tokens are fetched once, outside the retry loop: a failure there (most
// often the session not being ready) is not something retrying fixes.
func (c *Client) get(ctx context.Context, path string, q url.Values, dst any) error {
	dev, mut, err := c.tokens(ctx)
	if err != nil {
		return err
	}
	return retry.Do(ctx, retry.Default(), isRetryableErr, func() error {
		return c.doGet(ctx, dev, mut, path, q, dst)
	})
}

func (c *Client) doGet(ctx context.Context, dev, mut, path string, q url.Values, dst any) error {
	endpoint := path
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = ampBase + endpoint
	}
	if len(q) > 0 {
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		endpoint += sep + q.Encode()
	}

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+dev)
	req.Header.Set("Media-User-Token", mut)
	req.Header.Set("Origin", origin)
	req.Header.Set("Accept", "application/json")

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("apple catalog request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("decode apple catalog response: %w", err)
	}
	return nil
}

// apiError turns a non-200 into a typed error, preferring Apple's own detail.
func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var errs struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
			Status string `json:"status"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &errs)

	var title, detail string
	if len(errs.Errors) > 0 {
		title, detail = errs.Errors[0].Title, errs.Errors[0].Detail
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}

	apiErr := &APIError{Status: resp.StatusCode, Title: title, Detail: detail}
	if resp.StatusCode == http.StatusTooManyRequests {
		if s := strings.TrimSpace(resp.Header.Get("Retry-After")); s != "" {
			if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
				apiErr.retryAfter = time.Duration(secs) * time.Second
			}
		}
	}
	return apiErr
}
