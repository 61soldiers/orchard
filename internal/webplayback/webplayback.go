// Package webplayback fetches and decrypts the Widevine-protected AAC-256
// asset Apple serves through its web-playback API.
//
// It exists for the small set of older, AAC-only catalog items that have no
// FairPlay HLS rendition (see stream.ErrNoFairPlayAsset): the wrapper daemon
// resolves those to a legacy-DRM file it can't use, but Apple's web player
// still streams them as Widevine-encrypted fragmented MP4. This is the same
// path apple-music-downloader's `--aac aac-lc` mode takes (utils/runv3) —
// ported here, using github.com/iyear/gowidevine for the CDM and a bundled
// public L3 device (see device.go).
//
// The whole asset is downloaded into memory (a few MB for a 256 kbps track)
// and decrypted in one pass, so this is download-only — it does not stream
// fragment by fragment the way internal/stream does for FairPlay.
package webplayback

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/grafov/m3u8"
	widevine "github.com/iyear/gowidevine"
	wvpb "github.com/iyear/gowidevine/widevinepb"
	"google.golang.org/protobuf/proto"

	"orchard/internal/retry"
)

const (
	webPlaybackURL = "https://play.music.apple.com/WebObjects/MZPlay.woa/wa/webPlayback"
	licenseURL     = "https://play.itunes.apple.com/WebObjects/MZPlay.woa/wa/acquireWebPlaybackLicense"

	// aacFlavor is the AAC-256 CTR-protected asset in a webPlayback response.
	aacFlavor = "28:ctrp256"

	// widevineSystemID is the Widevine DRM system id, as raw bytes.
	widevineSystemID = "\xed\xef\x8b\xa9\x79\xd6\x4a\xce\xa3\xc8\x27\xdc\xd5\x1d\x21\xed"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// ErrNoAsset means Apple's web-playback response carried no usable AAC-256
// asset for the track — nothing here can be downloaded for it.
var ErrNoAsset = errors.New("no web-playback AAC asset for this track")

// Client talks to Apple's web-playback and Widevine license endpoints.
type Client struct {
	hc *http.Client
}

// New returns a Client.
func New() *Client {
	return &Client{
		// A track asset is a few MB; the whole thing is read in one pass.
		hc: &http.Client{Timeout: 5 * time.Minute},
	}
}

// PrepareAAC resolves and downloads the still-encrypted AAC-256 asset for a
// track, plus its content keys. Everything that can fail — the web-playback
// lookup, the license exchange, the CDN download — and every retry happens
// here, so a caller (the /stream handler) can commit an HTTP 200 the moment
// this returns cleanly and never time a client out mid-decrypt.
func (c *Client) PrepareAAC(ctx context.Context, adamID, devToken, musicToken string) (asset []byte, keys []*widevine.Key, err error) {
	assetURL, err := c.assetURL(ctx, adamID, devToken, musicToken)
	if err != nil {
		return nil, nil, err
	}

	fileURL, keyURI, err := c.resolvePlaylist(ctx, assetURL, devToken, musicToken)
	if err != nil {
		return nil, nil, err
	}

	kid, err := kidFromKeyURI(keyURI)
	if err != nil {
		return nil, nil, err
	}
	psshBox, err := buildPSSH(kid)
	if err != nil {
		return nil, nil, err
	}

	dev, err := device()
	if err != nil {
		return nil, nil, err
	}
	pssh, err := widevine.NewPSSH(psshBox)
	if err != nil {
		return nil, nil, fmt.Errorf("parse pssh: %w", err)
	}
	cdm := widevine.NewCDM(dev)
	challenge, parseLicense, err := cdm.GetLicenseChallenge(pssh, wvpb.LicenseType_STREAMING, false)
	if err != nil {
		return nil, nil, fmt.Errorf("build license challenge: %w", err)
	}

	licenseBytes, err := c.acquireLicense(ctx, adamID, keyURI, challenge, devToken, musicToken)
	if err != nil {
		return nil, nil, err
	}
	keys, err = parseLicense(licenseBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse license: %w", err)
	}

	asset, err = c.getBytes(ctx, fileURL, devToken, musicToken)
	if err != nil {
		return nil, nil, fmt.Errorf("download asset: %w", err)
	}
	return asset, keys, nil
}

// DecryptAAC writes the decrypted (still fragmented) MP4 for a PrepareAAC
// result to w. It is pure CPU over an in-memory buffer — no network — so it
// runs after a 200 has been committed.
func DecryptAAC(asset []byte, keys []*widevine.Key, w io.Writer) error {
	if err := widevine.DecryptMP4Auto(bytes.NewReader(asset), keys, w); err != nil {
		return fmt.Errorf("decrypt asset: %w", err)
	}
	return nil
}

// FetchAAC resolves, downloads and decrypts a track's AAC-256 asset, writing
// the decrypted fragmented MP4 to w. Used by the download pipeline.
func (c *Client) FetchAAC(ctx context.Context, adamID, devToken, musicToken string, w io.Writer) error {
	asset, keys, err := c.PrepareAAC(ctx, adamID, devToken, musicToken)
	if err != nil {
		return err
	}
	return DecryptAAC(asset, keys, w)
}

// httpErr is a non-200 from one of Apple's playback endpoints. It carries the
// status so isTransient can tell a retryable blip from a permanent refusal.
type httpErr struct {
	what string
	code int
}

func (e *httpErr) Error() string { return fmt.Sprintf("%s: apple returned %d", e.what, e.code) }

// isTransient reports whether an error is worth another attempt: a transport
// failure, a 429, or a 5xx. A well-formed 4xx (or a cancelled context) is not.
func isTransient(err error) bool {
	var he *httpErr
	if errors.As(err, &he) {
		return he.code == http.StatusTooManyRequests || he.code >= 500
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// send runs build() — which must produce a fresh request each call — with
// retry/backoff on transient failures and returns the response body.
func (c *Client) send(ctx context.Context, what string, build func() (*http.Request, error)) ([]byte, error) {
	var out []byte
	err := retry.Do(ctx, retry.Default(), isTransient, func() error {
		req, err := build()
		if err != nil {
			return err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &httpErr{what: what, code: resp.StatusCode}
		}
		out, err = io.ReadAll(resp.Body)
		return err
	})
	return out, err
}

// assetURL runs the web-playback request and returns the AAC-256 asset's
// playlist URL.
func (c *Client) assetURL(ctx context.Context, adamID, devToken, musicToken string) (string, error) {
	raw, err := c.send(ctx, "web-playback request", func() (*http.Request, error) {
		body, _ := json.Marshal(map[string]string{"salableAdamId": adamID})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webPlaybackURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.setPlaybackHeaders(req, devToken, musicToken)
		return req, nil
	})
	if err != nil {
		return "", err
	}

	var out struct {
		SongList []struct {
			Assets []struct {
				Flavor string `json:"flavor"`
				URL    string `json:"URL"`
			} `json:"assets"`
		} `json:"songList"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode web-playback response: %w", err)
	}
	if len(out.SongList) == 0 {
		return "", ErrNoAsset
	}
	for _, a := range out.SongList[0].Assets {
		if a.Flavor == aacFlavor && a.URL != "" {
			return a.URL, nil
		}
	}
	return "", ErrNoAsset
}

// resolvePlaylist fetches the media playlist and returns the byte-range asset
// URL and the verbatim EXT-X-KEY URI.
func (c *Client) resolvePlaylist(ctx context.Context, playlistURL, devToken, musicToken string) (fileURL, keyURI string, err error) {
	raw, err := c.getBytes(ctx, playlistURL, devToken, musicToken)
	if err != nil {
		return "", "", fmt.Errorf("fetch media playlist: %w", err)
	}
	pl, listType, err := m3u8.DecodeFrom(bytes.NewReader(raw), true)
	if err != nil {
		return "", "", fmt.Errorf("parse media playlist: %w", err)
	}
	if listType != m3u8.MEDIA {
		return "", "", errors.New("web-playback asset is not a media playlist")
	}
	media := pl.(*m3u8.MediaPlaylist)
	if media.Key == nil || media.Key.URI == "" {
		return "", "", errors.New("media playlist has no EXT-X-KEY")
	}
	if media.Map == nil || media.Map.URI == "" {
		return "", "", errors.New("media playlist has no EXT-X-MAP")
	}
	return resolveRef(playlistURL, media.Map.URI), media.Key.URI, nil
}

// acquireLicense POSTs the Widevine challenge in Apple's envelope and returns
// the raw license bytes.
func (c *Client) acquireLicense(ctx context.Context, adamID, keyURI string, challenge []byte, devToken, musicToken string) ([]byte, error) {
	raw, err := c.send(ctx, "license request", func() (*http.Request, error) {
		body, _ := json.Marshal(map[string]any{
			"challenge":      base64.StdEncoding.EncodeToString(challenge),
			"key-system":     "com.widevine.alpha",
			"uri":            keyURI,
			"adamId":         adamID,
			"isLibrary":      false,
			"user-initiated": true,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, licenseURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.setPlaybackHeaders(req, devToken, musicToken)
		return req, nil
	})
	if err != nil {
		return nil, err
	}

	var out struct {
		License   string `json:"license"`
		ErrorCode int    `json:"errorCode"`
		Status    int    `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode license response: %w", err)
	}
	if out.ErrorCode != 0 || out.Status != 0 {
		return nil, fmt.Errorf("license request rejected (errorCode %d, status %d)", out.ErrorCode, out.Status)
	}
	license, err := base64.StdEncoding.DecodeString(out.License)
	if err != nil {
		return nil, fmt.Errorf("decode license payload: %w", err)
	}
	return license, nil
}

func (c *Client) setPlaybackHeaders(req *http.Request, devToken, musicToken string) {
	req.Header.Set("Authorization", "Bearer "+devToken)
	req.Header.Set("x-apple-music-user-token", musicToken)
	req.Header.Set("Media-User-Token", musicToken)
	req.Header.Set("Origin", "https://music.apple.com")
	req.Header.Set("Referer", "https://music.apple.com/")
	req.Header.Set("User-Agent", userAgent)
}

func (c *Client) getBytes(ctx context.Context, url, devToken, musicToken string) ([]byte, error) {
	return c.send(ctx, "fetch "+url, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		c.setPlaybackHeaders(req, devToken, musicToken)
		return req, nil
	})
}

// kidFromKeyURI pulls the raw key id out of an EXT-X-KEY URI of the form
// "data:text/plain;base64,<base64 kid>".
func kidFromKeyURI(keyURI string) ([]byte, error) {
	_, b64, ok := strings.Cut(keyURI, ",")
	if !ok || b64 == "" {
		return nil, fmt.Errorf("unexpected key URI %q", keyURI)
	}
	kid, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode key id: %w", err)
	}
	return kid, nil
}

// buildPSSH assembles a version-0 Widevine PSSH box wrapping just the key id,
// which is all gowidevine's CDM needs to form the license challenge.
func buildPSSH(kid []byte) ([]byte, error) {
	data, err := proto.Marshal(&wvpb.WidevinePsshData{KeyIds: [][]byte{kid}})
	if err != nil {
		return nil, fmt.Errorf("marshal pssh data: %w", err)
	}
	box := new(bytes.Buffer)
	total := 8 + 4 + 16 + 4 + len(data) // header + verflags + systemID + dataLen + data
	binary.Write(box, binary.BigEndian, uint32(total))
	box.WriteString("pssh")
	binary.Write(box, binary.BigEndian, uint32(0)) // version 0, flags 0
	box.WriteString(widevineSystemID)
	binary.Write(box, binary.BigEndian, uint32(len(data)))
	box.Write(data)
	return box.Bytes(), nil
}

// resolveRef resolves a possibly-relative playlist reference against a base URL.
func resolveRef(base, ref string) string {
	if strings.HasPrefix(ref, "http") {
		return ref
	}
	if i := strings.LastIndex(base, "/"); i != -1 {
		return base[:i+1] + ref
	}
	return ref
}
