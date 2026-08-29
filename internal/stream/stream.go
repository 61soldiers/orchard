package stream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchard/internal/retry"
)

// Codec selects which audio variant to pull from a track's manifest.
type Codec string

const (
	CodecALAC  Codec = "alac"  // lossless, up to 24-bit
	CodecAtmos Codec = "atmos" // Dolby Atmos, ec-3
	CodecAAC   Codec = "aac"   // 256 kbps stereo
)

// ParseCodec validates a codec name.
func ParseCodec(s string) (Codec, error) {
	switch Codec(strings.ToLower(strings.TrimSpace(s))) {
	case "", CodecALAC:
		return CodecALAC, nil
	case CodecAtmos:
		return CodecAtmos, nil
	case CodecAAC:
		return CodecAAC, nil
	}
	return "", fmt.Errorf("unknown codec %q; use alac, atmos or aac", s)
}

// Variant is one audio rendition from a track's master playlist.
type Variant struct {
	Codec      Codec  `json:"codec"`
	GroupID    string `json:"groupId"`
	Bandwidth  int    `json:"bandwidth"`
	SampleRate int    `json:"sampleRate,omitempty"`
	BitDepth   int    `json:"bitDepth,omitempty"`
	URL        string `json:"-"`
}

// Config points the client at the daemon's services.
type Config struct {
	Host        string
	M3U8Port    int
	DecryptPort int
}

const (
	// manifestTimeout bounds the daemon's manifest lookup, which round-trips to
	// Apple and is routinely slow.
	manifestTimeout = 3 * time.Minute
	// manifestIdle is how long to wait for more bytes once the URL starts arriving.
	manifestIdle = 2 * time.Second
	// masterTTL caches resolved manifests. Apple's URLs are signed and expire, so
	// keep this well under their lifetime.
	masterTTL = 5 * time.Minute
)

type masterEntry struct {
	url     string
	expires time.Time
}

// Client resolves manifests and produces decrypted audio.
type Client struct {
	cfg Config
	hc  *http.Client

	mu      sync.Mutex
	masters map[string]masterEntry
}

// New returns a Client.
func New(cfg Config) *Client {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	return &Client{
		cfg:     cfg,
		masters: map[string]masterEntry{},
		// No overall timeout: a track download is a long streaming read.
		hc: &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}},
	}
}

// ErrNoVariant means the track has no rendition in the requested codec.
var ErrNoVariant = errors.New("track has no rendition in that codec")

// ErrNoFairPlayAsset means the daemon resolved the track to a single-file
// protected asset (streamingaudio.itunes.apple.com/.../mzaf_*.m4p) instead of
// an HLS master playlist, so the wrapper/FairPlay pipeline this package
// implements has nothing to decrypt.
//
// Seen for tracks with no lossless master — the small slice of the catalog
// Apple never re-encoded for FairPlay streaming delivery (some 2021-era
// game/film soundtracks, older or indie catalog). The asset the daemon hands
// back is the full track wrapped in legacy iTunes DRM (`stsd` = `drms`,
// `schm` = `itun`, confirmed by inspecting one), which nothing here can use.
//
// This is not a dead end: internal/webplayback fetches the same track's
// Widevine-protected AAC-256 asset (`flavor 28:ctrp256`) via Apple's
// web-playback API and decrypts it. Both download.Manager and the /stream
// handler fall back to that when they see this error, so streaming and
// downloading these tracks both work — just over the Widevine path.
var ErrNoFairPlayAsset = errors.New("no FairPlay HLS asset for this track (handled via the Widevine AAC fallback)")

// masterURL asks the daemon for a track's master playlist URL. The protocol is
// a length-prefixed adam id; the daemon writes back the URL but keeps the
// connection open, so this reads until the socket goes quiet rather than until
// EOF. Resolution involves a round trip to Apple and can take a minute.
func (c *Client) masterURL(ctx context.Context, adamID string) (string, error) {
	if adamID == "" || len(adamID) > 255 {
		return "", errors.New("invalid adam id")
	}

	c.mu.Lock()
	if e, ok := c.masters[adamID]; ok && time.Now().Before(e.expires) {
		u := e.url
		c.mu.Unlock()
		return u, nil
	}
	c.mu.Unlock()

	addr := net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.M3U8Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("reach the daemon's manifest service: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write(append([]byte{byte(len(adamID))}, adamID...)); err != nil {
		return "", err
	}

	raw, err := readUntilIdle(ctx, conn, manifestTimeout, manifestIdle, 8<<10)
	if err != nil && len(raw) == 0 {
		return "", fmt.Errorf("read manifest URL: %w", err)
	}

	got := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(got, "http") {
		return "", fmt.Errorf("daemon returned no manifest for track %s", adamID)
	}
	u := strings.Fields(got)[0]

	// A streamable track resolves to a "*_default.m3u8" HLS master playlist.
	// A track Apple never re-encoded for streaming resolves instead to a
	// single-file legacy-DRM asset (mzaf_*.m4p on streamingaudio.itunes.apple.com),
	// which parseMaster would later reduce to zero variants and an opaque "no
	// rendition" error. Fail here with a specific error, and don't cache it —
	// a re-request could succeed if the catalog is updated.
	if !strings.Contains(u, ".m3u8") {
		return "", ErrNoFairPlayAsset
	}

	c.mu.Lock()
	c.masters[adamID] = masterEntry{url: u, expires: time.Now().Add(masterTTL)}
	c.mu.Unlock()
	return u, nil
}

// readUntilIdle collects bytes until the peer stops sending. It waits up to
// first for the first byte, then idle between subsequent reads.
func readUntilIdle(ctx context.Context, conn net.Conn, first, idle time.Duration, max int) ([]byte, error) {
	deadline := time.Now().Add(first)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}

	var out []byte
	buf := make([]byte, 4<<10)
	for len(out) < max {
		n, err := conn.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
			// Got something; only wait a moment for more.
			if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
				return out, err
			}
			continue
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Variants lists the renditions available for a track.
func (c *Client) Variants(ctx context.Context, adamID string) ([]Variant, error) {
	master, err := c.masterURL(ctx, adamID)
	if err != nil {
		return nil, err
	}
	body, err := c.fetch(ctx, master)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseMaster(string(data), master), nil
}

// parseMaster reads EXT-X-MEDIA groups and the STREAM-INF lines that reference
// them. Audio renditions carry their URI on the STREAM-INF's following line.
func parseMaster(body, baseURL string) []Variant {
	type media struct{ sampleRate, bitDepth int }
	groups := map[string]media{}

	lines := strings.Split(body, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") || !strings.Contains(line, "TYPE=AUDIO") {
			continue
		}
		attrs := parseAttrs(line)
		m := media{}
		if v, err := strconv.Atoi(attrs["SAMPLE-RATE"]); err == nil {
			m.sampleRate = v
		}
		if v, err := strconv.Atoi(attrs["BIT-DEPTH"]); err == nil {
			m.bitDepth = v
		}
		groups[attrs["GROUP-ID"]] = m
	}

	var out []Variant
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		attrs := parseAttrs(line)

		var codec Codec
		switch {
		case strings.Contains(attrs["CODECS"], "alac"):
			codec = CodecALAC
		case strings.Contains(attrs["CODECS"], "ec-3"):
			codec = CodecAtmos
		case strings.Contains(attrs["CODECS"], "mp4a.40.2"):
			codec = CodecAAC
		default:
			continue // HE-AAC and binaural/downmix renditions are not offered
		}

		group := attrs["AUDIO"]
		// Skip the binaural and downmix variants of a codec.
		if strings.Contains(group, "binaural") || strings.Contains(group, "downmix") {
			continue
		}

		uri := ""
		for j := i + 1; j < len(lines); j++ {
			s := strings.TrimSpace(lines[j])
			if s == "" || strings.HasPrefix(s, "#") {
				continue
			}
			uri = s
			break
		}
		if uri == "" {
			continue
		}

		bw, _ := strconv.Atoi(attrs["AVERAGE-BANDWIDTH"])
		m := groups[group]
		out = append(out, Variant{
			Codec: codec, GroupID: group, Bandwidth: bw,
			SampleRate: m.sampleRate, BitDepth: m.bitDepth,
			URL: resolveRef(baseURL, uri),
		})
	}
	return out
}

// Pick returns the highest-bandwidth variant for a codec.
func Pick(variants []Variant, codec Codec) (Variant, bool) {
	var best Variant
	found := false
	for _, v := range variants {
		if v.Codec != codec {
			continue
		}
		if !found || v.Bandwidth > best.Bandwidth {
			best, found = v, true
		}
	}
	return best, found
}

// parseAttrs splits an EXT-X tag's comma-separated attribute list, honouring
// quoted values that may themselves contain commas.
func parseAttrs(line string) map[string]string {
	_, rest, _ := strings.Cut(line, ":")
	attrs := map[string]string{}

	var key, val strings.Builder
	inKey, inQuote := true, false
	flush := func() {
		if key.Len() > 0 {
			attrs[strings.TrimSpace(key.String())] = strings.Trim(strings.TrimSpace(val.String()), `"`)
		}
		key.Reset()
		val.Reset()
		inKey = true
	}

	for _, r := range rest {
		switch {
		case r == '"':
			inQuote = !inQuote
			val.WriteRune(r)
		case r == '=' && inKey && !inQuote:
			inKey = false
		case r == ',' && !inQuote:
			flush()
		case inKey:
			key.WriteRune(r)
		default:
			val.WriteRune(r)
		}
	}
	flush()
	return attrs
}

func resolveRef(base, ref string) string {
	if strings.HasPrefix(ref, "http") {
		return ref
	}
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// fetchStatusErr is a non-200 response from Apple's CDN. It is a distinct
// type (rather than a plain fmt.Errorf) so isRetryableFetchErr can tell a
// transient status from a permanent one.
type fetchStatusErr struct {
	url        string
	statusCode int
	statusText string
}

func (e *fetchStatusErr) Error() string {
	return fmt.Sprintf("GET %s returned %s", e.url, e.statusText)
}

func isRetryableFetchErr(err error) bool {
	var se *fetchStatusErr
	if errors.As(err, &se) {
		switch se.statusCode {
		case http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A transport-level failure (dial, TLS, timeout): exactly the transient
	// case retries exist for.
	return true
}

// fetch issues a GET against Apple's CDN, retrying a transient failure before
// any of the body has been read. Once fetch returns, the caller owns a live
// stream and further failures are not retried — resuming mid-track is out of
// scope.
func (c *Client) fetch(ctx context.Context, u string) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := retry.Do(ctx, retry.Default(), isRetryableFetchErr, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			code, text := resp.StatusCode, resp.Status
			resp.Body.Close()
			return &fetchStatusErr{url: u, statusCode: code, statusText: text}
		}
		body = resp.Body
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Open decrypts a track and writes a playable fragmented MP4 to w.
//
// Audio arrives as one long byte-range asset, so this is a streaming transform:
// each moof/mdat fragment is decrypted and flushed as soon as it has been read,
// which is what makes progressive playback possible. It never buffers the whole
// track.
// Progress reports how many bytes of the encrypted asset have been consumed.
// total is 0 when the server does not declare a length.
type Progress func(done, total int64)

// Open decrypts a track and writes a playable fragmented MP4 to w. onProgress
// may be nil.
func (c *Client) Open(ctx context.Context, adamID string, v Variant, w io.Writer, onProgress Progress) error {
	playlist, err := c.fetch(ctx, v.URL)
	if err != nil {
		return err
	}
	segments, err := parseMediaPlaylist(playlist)
	playlist.Close()
	if err != nil {
		return err
	}
	if len(segments) == 0 || segments[0] == nil {
		return errors.New("media playlist has no segments")
	}
	if segments[0].Limit <= 0 {
		return errors.New("playlist is not byte-range based, which is unsupported")
	}

	assetURL := resolveRef(v.URL, segments[0].URI)
	body, err := c.fetch(ctx, assetURL)
	if err != nil {
		return err
	}
	defer body.Close()

	// Total comes from the playlist's byte ranges, which cover the whole asset.
	var total int64
	for _, seg := range segments {
		if seg != nil {
			total += int64(seg.Limit)
		}
	}
	var source io.Reader = body
	if onProgress != nil {
		source = &countingReader{r: body, total: total, report: onProgress}
	}

	addr := net.JoinHostPort(c.cfg.Host, strconv.Itoa(c.cfg.DecryptPort))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("reach the daemon's decryption service: %w", err)
	}
	defer closeDecryptor(conn)

	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	in := bufio.NewReaderSize(source, 1<<20)

	init, offset, err := readInitSegment(in)
	if err != nil {
		return fmt.Errorf("read init segment: %w", err)
	}
	tracks, err := transformInit(init)
	if err != nil {
		return fmt.Errorf("read decryption info: %w", err)
	}
	// Deliberately not calling sanitizeInit here. Dropping the duplicate stsd
	// entry breaks fragments whose trun references sample_description_index 2:
	// demuxers then stop after the first fragment. Upstream can get away with it
	// because MP4Box rebuilds the sample table afterwards; a live stream cannot.
	if err := init.Encode(w); err != nil {
		return err
	}
	flush(w)

	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		frag, next, err := readNextFragment(in, offset)
		if err != nil {
			return fmt.Errorf("read fragment %d: %w", i, err)
		}
		if frag == nil {
			return nil // clean EOF
		}
		offset = next

		if i >= len(segments) || segments[i] == nil {
			return fmt.Errorf("fragment %d has no matching playlist segment", i)
		}
		// Apple rotates keys mid-track: the first segment uses a placeholder the
		// daemon expects under adam id "0", the rest use the real content key.
		if key := segments[i].Key; key != nil {
			if i != 0 {
				if err := switchKeys(rw); err != nil {
					return err
				}
			}
			id := adamID
			if key.URI == prefetchKey {
				id = "0"
			}
			if err := sendString(rw, id); err != nil {
				return err
			}
			if err := sendString(rw, key.URI); err != nil {
				return err
			}
		}

		if err := decryptFragment(frag, tracks, rw); err != nil {
			return fmt.Errorf("decrypt fragment %d: %w", i, err)
		}
		if err := frag.Encode(w); err != nil {
			return err
		}
		flush(w)
	}
}

// flush pushes bytes to the client as soon as a fragment is ready, so playback
// can start before the track finishes.
func flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// countingReader reports download progress, throttled so a fast transfer does
// not generate thousands of updates.
type countingReader struct {
	r      io.Reader
	done   int64
	total  int64
	last   time.Time
	report Progress
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if now := time.Now(); now.Sub(c.last) > 250*time.Millisecond || err != nil {
		c.last = now
		c.report(c.done, c.total)
	}
	return n, err
}
