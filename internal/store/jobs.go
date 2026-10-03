package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Job states.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// Per-track phases, in the order they occur.
const (
	PhaseQueued   = "queued"
	PhaseManifest = "manifest"
	PhaseDownload = "download"
	PhaseDecrypt  = "decrypt"
	PhaseRemux    = "remux"
	PhaseTag      = "tag"
	PhaseDone     = "done"
	PhaseFailed   = "failed"
)

// JobTrack is one track's progress within a job.
type JobTrack struct {
	TrackID    string    `json:"trackId"`
	Position   int       `json:"position"`
	Phase      string    `json:"phase"`
	BytesDone  int64     `json:"bytesDone"`
	BytesTotal int64     `json:"bytesTotal"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Job is a download request covering one or more tracks.
type Job struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	CatalogID  string     `json:"catalogId"`
	Codec      string     `json:"codec"`
	AlacMax    int        `json:"alacMax,omitempty"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Tracks     []JobTrack `json:"tracks"`
}

// Track is a downloaded file in the library.
type Track struct {
	ID           string    `json:"id"`
	AlbumID      string    `json:"albumId,omitempty"`
	Title        string    `json:"title"`
	Artist       string    `json:"artist,omitempty"`
	Album        string    `json:"album,omitempty"`
	DiscNumber   int       `json:"discNumber,omitempty"`
	TrackNumber  int       `json:"trackNumber,omitempty"`
	DurationMs   int       `json:"durationMs,omitempty"`
	Codec        string    `json:"codec,omitempty"`
	SampleRate   int       `json:"sampleRate,omitempty"`
	BitDepth     int       `json:"bitDepth,omitempty"`
	Path         string    `json:"-"`
	SizeBytes    int64     `json:"sizeBytes"`
	DownloadedAt time.Time `json:"downloadedAt"`
}

// CreateJob inserts a job and its track rows in one transaction.
func (s *Store) CreateJob(ctx context.Context, j Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (id, type, catalog_id, codec, alac_max, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		j.ID, j.Type, j.CatalogID, j.Codec, nullInt(j.AlacMax), j.State, formatTime(j.CreatedAt)); err != nil {
		return fmt.Errorf("insert job: %w", err)
	}

	now := formatTime(time.Now())
	for i, t := range j.Tracks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO job_tracks (job_id, track_id, position, phase, updated_at)
			 VALUES (?, ?, ?, ?, ?)`,
			j.ID, t.TrackID, i, PhaseQueued, now); err != nil {
			return fmt.Errorf("insert job track: %w", err)
		}
	}
	return tx.Commit()
}

// Job loads a job with its tracks.
func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	var alacMax sql.NullInt64
	var errMsg, startedAt, finishedAt sql.NullString
	var createdAt string

	err := s.db.QueryRowContext(ctx,
		`SELECT id, type, catalog_id, codec, alac_max, state, error, created_at, started_at, finished_at
		 FROM jobs WHERE id = ?`, id).
		Scan(&j.ID, &j.Type, &j.CatalogID, &j.Codec, &alacMax, &j.State, &errMsg,
			&createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}

	j.AlacMax = int(alacMax.Int64)
	j.Error = errMsg.String
	j.CreatedAt = parseTime(createdAt)
	j.StartedAt = parseTimePtr(startedAt)
	j.FinishedAt = parseTimePtr(finishedAt)

	tracks, err := s.jobTracks(ctx, id)
	if err != nil {
		return Job{}, err
	}
	j.Tracks = tracks
	return j, nil
}

func (s *Store) jobTracks(ctx context.Context, jobID string) ([]JobTrack, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT track_id, position, phase, bytes_done, bytes_total, error, updated_at
		 FROM job_tracks WHERE job_id = ? ORDER BY position`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobTrack
	for rows.Next() {
		var t JobTrack
		var errMsg sql.NullString
		var updated string
		if err := rows.Scan(&t.TrackID, &t.Position, &t.Phase, &t.BytesDone,
			&t.BytesTotal, &errMsg, &updated); err != nil {
			return nil, err
		}
		t.Error = errMsg.String
		t.UpdatedAt = parseTime(updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Jobs lists recent jobs, newest first.
func (s *Store) Jobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		j, err := s.Job(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// SetJobState moves a job between states, stamping the matching timestamp.
func (s *Store) SetJobState(ctx context.Context, id, state, errMsg string) error {
	now := formatTime(time.Now())
	var err error
	switch state {
	case JobRunning:
		_, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET state = ?, started_at = ? WHERE id = ?`, state, now, id)
	case JobDone, JobFailed, JobCancelled:
		_, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET state = ?, error = ?, finished_at = ? WHERE id = ?`,
			state, nullString(errMsg), now, id)
	default:
		_, err = s.db.ExecContext(ctx, `UPDATE jobs SET state = ? WHERE id = ?`, state, id)
	}
	return err
}

// UpdateJobTrack records progress for one track.
func (s *Store) UpdateJobTrack(ctx context.Context, jobID, trackID, phase string, done, total int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE job_tracks
		 SET phase = ?, bytes_done = ?, bytes_total = ?, error = ?, updated_at = ?
		 WHERE job_id = ? AND track_id = ?`,
		phase, done, total, nullString(errMsg), formatTime(time.Now()), jobID, trackID)
	return err
}

// ResumeInterrupted marks jobs left running by a crash as failed, so the queue
// does not show work that will never progress.
func (s *Store) ResumeInterrupted(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, error = ?, finished_at = ?
		 WHERE state IN (?, ?)`,
		JobFailed, "interrupted by a server restart", formatTime(time.Now()),
		JobRunning, JobQueued)
	return err
}

// UpsertTrack records a downloaded file, replacing any earlier copy.
func (s *Store) UpsertTrack(ctx context.Context, t Track) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tracks (id, album_id, title, artist, album, disc_number, track_number,
		                     duration_ms, codec, sample_rate, bit_depth, path, size_bytes, downloaded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   album_id = excluded.album_id, title = excluded.title, artist = excluded.artist,
		   album = excluded.album, disc_number = excluded.disc_number,
		   track_number = excluded.track_number, duration_ms = excluded.duration_ms,
		   codec = excluded.codec, sample_rate = excluded.sample_rate,
		   bit_depth = excluded.bit_depth, path = excluded.path,
		   size_bytes = excluded.size_bytes, downloaded_at = excluded.downloaded_at`,
		t.ID, nullString(t.AlbumID), t.Title, nullString(t.Artist), nullString(t.Album),
		nullInt(t.DiscNumber), nullInt(t.TrackNumber), nullInt(t.DurationMs),
		nullString(t.Codec), nullInt(t.SampleRate), nullInt(t.BitDepth),
		t.Path, t.SizeBytes, formatTime(t.DownloadedAt))
	return err
}

// Track loads one library entry.
func (s *Store) Track(ctx context.Context, id string) (Track, error) {
	row := s.db.QueryRowContext(ctx, trackSelect+` WHERE id = ?`, id)
	t, err := scanTrack(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Track{}, ErrNotFound
	}
	return t, err
}

// TrackFilter narrows and orders a library listing. The zero value lists
// everything, newest-download-last — the same behaviour TracksSince always
// had, which sync clients depend on.
type TrackFilter struct {
	// Since keeps sync clients working: only tracks downloaded at or after it.
	Since time.Time
	// Q matches a substring of title, artist or album, case-insensitively.
	Q       string
	Artist  string
	Album   string
	AlbumID string
	Codec   string
	// Sort is one of "downloadedAt" (default), "title", "artist", "album",
	// "duration" or "size".
	Sort string
	Desc bool
	// Limit defaults to 500 and is capped at 1000. Offset defaults to 0.
	Limit  int
	Offset int
}

// Tracks lists library entries matching f.
func (s *Store) Tracks(ctx context.Context, f TrackFilter) ([]Track, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var where []string
	var args []any
	if !f.Since.IsZero() {
		where = append(where, "downloaded_at >= ?")
		args = append(args, formatTime(f.Since))
	}
	if f.Artist != "" {
		where = append(where, "artist = ? COLLATE NOCASE")
		args = append(args, f.Artist)
	}
	if f.Album != "" {
		where = append(where, "album = ? COLLATE NOCASE")
		args = append(args, f.Album)
	}
	if f.AlbumID != "" {
		where = append(where, "album_id = ?")
		args = append(args, f.AlbumID)
	}
	if f.Codec != "" {
		where = append(where, "codec = ? COLLATE NOCASE")
		args = append(args, f.Codec)
	}
	if f.Q != "" {
		where = append(where, "(title LIKE ? ESCAPE '\\' OR artist LIKE ? ESCAPE '\\' OR album LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(f.Q) + "%"
		args = append(args, like, like, like)
	}

	query := trackSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + sortColumn(f.Sort)
	if f.Desc {
		query += " DESC"
	} else {
		query += " ASC"
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Track
	for rows.Next() {
		t, err := scanTrack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func sortColumn(s string) string {
	switch s {
	case "title":
		return "title COLLATE NOCASE"
	case "artist":
		return "artist COLLATE NOCASE"
	case "album":
		return "album COLLATE NOCASE"
	case "duration":
		return "duration_ms"
	case "size":
		return "size_bytes"
	default:
		return "downloaded_at"
	}
}

// escapeLike escapes LIKE metacharacters so a search term is matched
// literally rather than as a pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const trackSelect = `SELECT id, album_id, title, artist, album, disc_number, track_number,
	duration_ms, codec, sample_rate, bit_depth, path, size_bytes, downloaded_at FROM tracks`

// albumGroupKey groups by the stable catalog album id when one was recorded,
// falling back to the album name for tracks downloaded before that field
// existed.
const albumGroupKey = "CASE WHEN album_id IS NOT NULL AND album_id != '' THEN album_id ELSE 'name:' || COALESCE(album, '') END"

// AlbumSummary aggregates the tracks the library holds for one album.
type AlbumSummary struct {
	AlbumID      string    `json:"albumId,omitempty"`
	Album        string    `json:"album"`
	Artist       string    `json:"artist,omitempty"`
	TrackCount   int       `json:"trackCount"`
	DurationMs   int64     `json:"durationMs"`
	SizeBytes    int64     `json:"sizeBytes"`
	DownloadedAt time.Time `json:"downloadedAt"`
}

// Albums groups downloaded tracks by album, newest download first.
func (s *Store) Albums(ctx context.Context) ([]AlbumSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(album_id, ''), '') AS album_id,
			COALESCE(NULLIF(album, ''), '(unknown album)') AS album,
			CASE WHEN COUNT(DISTINCT COALESCE(artist, '')) > 1 THEN 'Various Artists' ELSE MIN(artist) END AS artist,
			COUNT(*) AS track_count,
			COALESCE(SUM(duration_ms), 0) AS duration_ms,
			COALESCE(SUM(size_bytes), 0) AS size_bytes,
			MAX(downloaded_at) AS downloaded_at
		FROM tracks
		GROUP BY `+albumGroupKey+`
		ORDER BY downloaded_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AlbumSummary
	for rows.Next() {
		var a AlbumSummary
		var artist sql.NullString
		var downloaded string
		if err := rows.Scan(&a.AlbumID, &a.Album, &artist, &a.TrackCount,
			&a.DurationMs, &a.SizeBytes, &downloaded); err != nil {
			return nil, err
		}
		a.Artist = artist.String
		a.DownloadedAt = parseTime(downloaded)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ArtistSummary aggregates the tracks the library holds for one artist.
type ArtistSummary struct {
	Artist     string `json:"artist"`
	AlbumCount int    `json:"albumCount"`
	TrackCount int    `json:"trackCount"`
	SizeBytes  int64  `json:"sizeBytes"`
}

// Artists groups downloaded tracks by artist, alphabetically.
func (s *Store) Artists(ctx context.Context) ([]ArtistSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(artist, ''), '(unknown artist)') AS artist,
			COUNT(DISTINCT `+albumGroupKey+`) AS album_count,
			COUNT(*) AS track_count,
			COALESCE(SUM(size_bytes), 0) AS size_bytes
		FROM tracks
		GROUP BY COALESCE(NULLIF(artist, ''), '(unknown artist)')
		ORDER BY artist COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ArtistSummary
	for rows.Next() {
		var a ArtistSummary
		if err := rows.Scan(&a.Artist, &a.AlbumCount, &a.TrackCount, &a.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LibraryStats summarises the whole library, for a client's storage/settings view.
type LibraryStats struct {
	TrackCount      int   `json:"trackCount"`
	AlbumCount      int   `json:"albumCount"`
	ArtistCount     int   `json:"artistCount"`
	TotalSizeBytes  int64 `json:"totalSizeBytes"`
	TotalDurationMs int64 `json:"totalDurationMs"`
}

// Stats summarises the whole library.
func (s *Store) Stats(ctx context.Context) (LibraryStats, error) {
	var st LibraryStats
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(DISTINCT `+albumGroupKey+`),
			COUNT(DISTINCT COALESCE(NULLIF(artist, ''), '(unknown artist)')),
			COALESCE(SUM(size_bytes), 0),
			COALESCE(SUM(duration_ms), 0)
		FROM tracks`).
		Scan(&st.TrackCount, &st.AlbumCount, &st.ArtistCount, &st.TotalSizeBytes, &st.TotalDurationMs)
	return st, err
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanTrack(sc scanner) (Track, error) {
	var t Track
	var albumID, artist, album, codec sql.NullString
	var disc, track, dur, rate, depth sql.NullInt64
	var downloaded string

	if err := sc.Scan(&t.ID, &albumID, &t.Title, &artist, &album, &disc, &track,
		&dur, &codec, &rate, &depth, &t.Path, &t.SizeBytes, &downloaded); err != nil {
		return Track{}, err
	}
	t.AlbumID, t.Artist, t.Album, t.Codec = albumID.String, artist.String, album.String, codec.String
	t.DiscNumber, t.TrackNumber = int(disc.Int64), int(track.Int64)
	t.DurationMs, t.SampleRate, t.BitDepth = int(dur.Int64), int(rate.Int64), int(depth.Int64)
	t.DownloadedAt = parseTime(downloaded)
	return t, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	return &t
}
