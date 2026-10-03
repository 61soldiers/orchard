// Package download turns catalog requests into decrypted files on disk.
//
// A job covers one catalog item (a song, album, artist or playlist). Tracks are
// processed one at a time by a single worker: the daemon's decryption service
// handles one connection at a time, so parallelism there buys nothing.
package download

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchard/internal/catalog"
	"orchard/internal/store"
	"orchard/internal/stream"
	"orchard/internal/webplayback"
)

// Event is a progress update broadcast to SSE subscribers.
type Event struct {
	JobID      string `json:"jobId"`
	State      string `json:"state,omitempty"`
	TrackID    string `json:"trackId,omitempty"`
	Phase      string `json:"phase,omitempty"`
	BytesDone  int64  `json:"bytesDone,omitempty"`
	BytesTotal int64  `json:"bytesTotal,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Request describes what to download.
type Request struct {
	Type      string   // song | album | artist | playlist
	CatalogID string   // the Apple catalog id
	Codec     string   // alac | atmos | aac
	TrackIDs  []string // optional subset, for album and playlist jobs
}

// Config wires the manager to its dependencies.
type Config struct {
	LibraryDir string
	FFmpegPath string
	QueueSize  int
}

// Manager owns the job queue.
type Manager struct {
	store   *store.Store
	stream  *stream.Client
	catalog *catalog.Client
	tokens  catalog.TokenSource
	wp      *webplayback.Client
	cfg     Config

	queue chan string

	mu      sync.Mutex
	subs    map[string]map[chan Event]struct{}
	cancels map[string]context.CancelFunc
}

// ErrUnknownType is returned for an unsupported request type.
var ErrUnknownType = errors.New("type must be song, album, artist or playlist")

// New returns a Manager. tokens supplies the developer + media-user tokens
// for the Widevine web-playback fallback (wp), used for AAC-only tracks that
// have no FairPlay HLS rendition.
func New(st *store.Store, str *stream.Client, cat *catalog.Client, tokens catalog.TokenSource, wp *webplayback.Client, cfg Config) *Manager {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 64
	}
	return &Manager{
		store: st, stream: str, catalog: cat, tokens: tokens, wp: wp, cfg: cfg,
		queue:   make(chan string, cfg.QueueSize),
		subs:    map[string]map[chan Event]struct{}{},
		cancels: map[string]context.CancelFunc{},
	}
}

// Run processes queued jobs until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-m.queue:
			m.runJob(ctx, id)
		}
	}
}

// Enqueue expands a catalog item into tracks and queues the job.
func (m *Manager) Enqueue(ctx context.Context, req Request) (store.Job, error) {
	codec, err := stream.ParseCodec(req.Codec)
	if err != nil {
		return store.Job{}, err
	}

	trackIDs, err := m.expand(ctx, req)
	if err != nil {
		return store.Job{}, err
	}
	if len(trackIDs) == 0 {
		return store.Job{}, errors.New("nothing to download for that item")
	}

	job := store.Job{
		ID:        newID(),
		Type:      req.Type,
		CatalogID: req.CatalogID,
		Codec:     string(codec),
		State:     store.JobQueued,
		CreatedAt: time.Now(),
	}
	for _, id := range trackIDs {
		job.Tracks = append(job.Tracks, store.JobTrack{TrackID: id, Phase: store.PhaseQueued})
	}
	if err := m.store.CreateJob(ctx, job); err != nil {
		return store.Job{}, err
	}

	select {
	case m.queue <- job.ID:
	default:
		_ = m.store.SetJobState(ctx, job.ID, store.JobFailed, "the download queue is full")
		return store.Job{}, errors.New("the download queue is full")
	}
	return job, nil
}

// expand resolves a catalog item to the track ids it contains.
func (m *Manager) expand(ctx context.Context, req Request) ([]string, error) {
	want := map[string]bool{}
	for _, id := range req.TrackIDs {
		want[id] = true
	}
	keep := func(ids []string) []string {
		if len(want) == 0 {
			return ids
		}
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if want[id] {
				out = append(out, id)
			}
		}
		return out
	}

	switch req.Type {
	case "song":
		return []string{req.CatalogID}, nil

	case "album":
		album, err := m.catalog.Album(ctx, req.CatalogID)
		if err != nil {
			return nil, err
		}
		return keep(songIDs(album.Tracks)), nil

	case "playlist":
		pl, err := m.catalog.PlaylistFull(ctx, req.CatalogID)
		if err != nil {
			return nil, err
		}
		return keep(songIDs(pl.Tracks)), nil

	case "artist":
		artist, err := m.catalog.Artist(ctx, req.CatalogID)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, al := range artist.Albums {
			full, err := m.catalog.Album(ctx, al.ID)
			if err != nil {
				return nil, err
			}
			ids = append(ids, songIDs(full.Tracks)...)
		}
		return keep(ids), nil
	}
	return nil, ErrUnknownType
}

func songIDs(songs []catalog.Song) []string {
	out := make([]string, 0, len(songs))
	for _, s := range songs {
		out = append(out, s.ID)
	}
	return out
}

// Cancel stops a running job.
func (m *Manager) Cancel(ctx context.Context, jobID string) error {
	m.mu.Lock()
	cancel, running := m.cancels[jobID]
	m.mu.Unlock()

	if running {
		cancel()
		return nil
	}

	job, err := m.store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if job.State != store.JobQueued {
		return fmt.Errorf("job is %s and cannot be cancelled", job.State)
	}
	return m.store.SetJobState(ctx, jobID, store.JobCancelled, "cancelled before it started")
}

// Subscribe returns a channel of events for a job, plus an unsubscribe func.
func (m *Manager) Subscribe(jobID string) (<-chan Event, func()) {
	ch := make(chan Event, 32)
	m.mu.Lock()
	if m.subs[jobID] == nil {
		m.subs[jobID] = map[chan Event]struct{}{}
	}
	m.subs[jobID][ch] = struct{}{}
	m.mu.Unlock()

	return ch, func() {
		m.mu.Lock()
		delete(m.subs[jobID], ch)
		if len(m.subs[jobID]) == 0 {
			delete(m.subs, jobID)
		}
		m.mu.Unlock()
		close(ch)
	}
}

// publish sends an event to subscribers, dropping it for any that are behind.
func (m *Manager) publish(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs[e.JobID] {
		select {
		case ch <- e:
		default:
		}
	}
}

func (m *Manager) runJob(parent context.Context, jobID string) {
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	m.cancels[jobID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.cancels, jobID)
		m.mu.Unlock()
	}()

	job, err := m.store.Job(ctx, jobID)
	if err != nil {
		slog.Error("load job", "job", jobID, "error", err)
		return
	}
	if job.State == store.JobCancelled {
		return
	}

	_ = m.store.SetJobState(ctx, jobID, store.JobRunning, "")
	m.publish(Event{JobID: jobID, State: store.JobRunning})
	slog.Info("job started", "job", jobID, "type", job.Type, "tracks", len(job.Tracks))

	var failed int
	for _, t := range job.Tracks {
		if ctx.Err() != nil {
			break
		}
		if err := m.runTrack(ctx, job, t.TrackID); err != nil {
			failed++
			msg := err.Error()
			slog.Warn("track failed", "job", jobID, "track", t.TrackID, "error", msg)
			_ = m.store.UpdateJobTrack(ctx, jobID, t.TrackID, store.PhaseFailed, 0, 0, msg)
			m.publish(Event{JobID: jobID, TrackID: t.TrackID, Phase: store.PhaseFailed, Error: msg})
		}
	}

	switch {
	case ctx.Err() != nil && parent.Err() == nil:
		_ = m.store.SetJobState(context.WithoutCancel(ctx), jobID, store.JobCancelled, "cancelled")
		m.publish(Event{JobID: jobID, State: store.JobCancelled})
	case failed == len(job.Tracks):
		_ = m.store.SetJobState(ctx, jobID, store.JobFailed, "every track failed")
		m.publish(Event{JobID: jobID, State: store.JobFailed, Error: "every track failed"})
	default:
		msg := ""
		if failed > 0 {
			msg = fmt.Sprintf("%d of %d tracks failed", failed, len(job.Tracks))
		}
		_ = m.store.SetJobState(ctx, jobID, store.JobDone, msg)
		m.publish(Event{JobID: jobID, State: store.JobDone, Error: msg})
	}
	slog.Info("job finished", "job", jobID, "failed", failed)
}

// runTrack takes one track from manifest to a tagged file in the library.
func (m *Manager) runTrack(ctx context.Context, job store.Job, trackID string) error {
	setPhase := func(phase string, done, total int64) {
		_ = m.store.UpdateJobTrack(ctx, job.ID, trackID, phase, done, total, "")
		m.publish(Event{JobID: job.ID, TrackID: trackID, Phase: phase, BytesDone: done, BytesTotal: total})
	}

	setPhase(store.PhaseManifest, 0, 0)
	song, err := m.catalog.Song(ctx, trackID)
	if err != nil {
		return fmt.Errorf("look up track: %w", err)
	}
	codec := job.Codec
	var variant stream.Variant
	viaWebPlayback := false
	variants, err := m.stream.Variants(ctx, trackID)
	if err != nil {
		if !errors.Is(err, stream.ErrNoFairPlayAsset) {
			return fmt.Errorf("resolve manifest: %w", err)
		}
		// No FairPlay HLS rendition — an older AAC-only catalog item. Fall
		// back to Apple's Widevine web-playback AAC-256 asset, which is
		// download-only (fetched whole, then decrypted).
		viaWebPlayback = true
		codec = string(stream.CodecAAC)
	} else {
		var ok bool
		variant, ok = stream.Pick(variants, stream.Codec(job.Codec))
		if !ok {
			return fmt.Errorf("no %s rendition for this track", job.Codec)
		}
	}

	tmpDir, err := os.MkdirTemp("", "orchard-dl-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	rawPath := filepath.Join(tmpDir, "raw.mp4")
	raw, err := os.Create(rawPath)
	if err != nil {
		return err
	}

	setPhase(store.PhaseDownload, 0, 0)
	if viaWebPlayback {
		var dev, mut string
		if dev, mut, err = m.tokens(ctx); err == nil {
			err = m.wp.FetchAAC(ctx, trackID, dev, mut, raw)
		}
	} else {
		err = m.stream.Open(ctx, trackID, variant, raw, func(done, total int64) {
			setPhase(store.PhaseDecrypt, done, total)
		})
	}
	closeErr := raw.Close()
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}

	setPhase(store.PhaseRemux, 0, 0)
	coverPath := m.fetchCover(ctx, song, tmpDir)
	outPath := filepath.Join(tmpDir, "out.m4a")
	if err := m.remux(ctx, rawPath, coverPath, outPath, song); err != nil {
		return err
	}

	setPhase(store.PhaseTag, 0, 0)
	albumDir := sanitise(song.AlbumName)
	if albumDir == "" {
		albumDir = "unknown-album"
	}
	relDir := filepath.Join(albumDir)
	finalDir := filepath.Join(m.cfg.LibraryDir, relDir)
	if err := os.MkdirAll(finalDir, 0o750); err != nil {
		return err
	}

	name := fmt.Sprintf("%02d - %s.m4a", song.TrackNumber, sanitise(song.Name))
	if song.TrackNumber == 0 {
		name = sanitise(song.Name) + ".m4a"
	}
	relPath := filepath.Join(relDir, name)
	finalPath := filepath.Join(m.cfg.LibraryDir, relPath)

	if err := moveFile(outPath, finalPath); err != nil {
		return err
	}
	m.fetchLyrics(ctx, song, finalPath)

	info, err := os.Stat(finalPath)
	if err != nil {
		return err
	}
	rec := store.Track{
		ID: trackID, AlbumID: song.AlbumID, Title: song.Name, Artist: song.ArtistName, Album: song.AlbumName,
		DiscNumber: song.DiscNumber, TrackNumber: song.TrackNumber, DurationMs: song.DurationMs,
		Codec: codec, SampleRate: variant.SampleRate, BitDepth: variant.BitDepth,
		Path: relPath, SizeBytes: info.Size(), DownloadedAt: time.Now(),
	}
	if err := m.store.UpsertTrack(ctx, rec); err != nil {
		return err
	}

	setPhase(store.PhaseDone, info.Size(), info.Size())
	return nil
}

// remux converts the decrypted fragmented MP4 into a progressive .m4a, adding
// tags and cover art in the same pass. Stream copy only, so audio is untouched.
func (m *Manager) remux(ctx context.Context, in, cover, out string, song *catalog.Song) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in}
	if cover != "" {
		args = append(args, "-i", cover, "-map", "0:a", "-map", "1:v",
			"-disposition:v:0", "attached_pic")
	} else {
		args = append(args, "-map", "0:a")
	}
	args = append(args, "-c", "copy")

	meta := map[string]string{
		"title":        song.Name,
		"artist":       song.ArtistName,
		"album":        song.AlbumName,
		"album_artist": song.ArtistName,
		"composer":     song.ComposerName,
		"date":         song.ReleaseDate,
		"genre":        strings.Join(song.Genres, "; "),
	}
	if song.TrackNumber > 0 {
		meta["track"] = strconv.Itoa(song.TrackNumber)
	}
	if song.DiscNumber > 0 {
		meta["disc"] = strconv.Itoa(song.DiscNumber)
	}
	for k, v := range meta {
		if v != "" {
			args = append(args, "-metadata", k+"="+v)
		}
	}
	args = append(args, out)

	cmd := exec.CommandContext(ctx, m.cfg.FFmpegPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("remux: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// fetchLyrics saves a `.lrc` sidecar next to audioPath, at the best sync
// tier catalog.Lyrics could produce (word-by-word, falling back to
// line-level, falling back to plain text — see catalog.ttmlToLRC). Lyrics
// are a nicety, same as cover art: no attempt when Apple says there are
// none, and any failure is silent rather than failing the download.
func (m *Manager) fetchLyrics(ctx context.Context, song *catalog.Song, audioPath string) {
	if !song.HasLyrics {
		return
	}
	lyrics, err := m.catalog.Lyrics(ctx, song.ID)
	if err != nil || lyrics.LRC == "" {
		return
	}
	lrcPath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".lrc"
	_ = os.WriteFile(lrcPath, []byte(lyrics.LRC), 0o644)
}

// fetchCover downloads album art, returning "" if it is unavailable. Artwork is
// a nicety, so failures are not fatal.
func (m *Manager) fetchCover(ctx context.Context, song *catalog.Song, dir string) string {
	if song.Artwork == nil || song.Artwork.URL == "" {
		return ""
	}
	url := catalog.ArtworkURL(song.Artwork.URL, 1200, 1200)

	req, err := newRequest(ctx, url)
	if err != nil {
		return ""
	}
	resp, err := httpDo(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}

	path := filepath.Join(dir, "cover.jpg")
	f, err := os.Create(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 8<<20)); err != nil {
		return ""
	}
	return path
}

// moveFile renames, falling back to a copy when crossing filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// sanitise makes a string safe for a path segment on any filesystem.
func sanitise(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', 0:
			b.WriteByte('_')
		default:
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
