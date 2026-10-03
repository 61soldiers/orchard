-- Orchard is single-tenant: one API key, one Apple session. There are no user
-- accounts, so jobs and tracks are not scoped to an owner.
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL CHECK (
        type IN (
            'song',
            'album',
            'artist',
            'playlist'
        )
    ),
    catalog_id TEXT NOT NULL,
    codec TEXT NOT NULL,
    alac_max INTEGER,
    state TEXT NOT NULL CHECK (
        state IN (
            'queued',
            'running',
            'done',
            'failed',
            'cancelled'
        )
    ),
    error TEXT,
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);

CREATE INDEX idx_jobs_created ON jobs (created_at DESC);

CREATE TABLE job_tracks (
    job_id TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    track_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    phase TEXT NOT NULL CHECK (
        phase IN (
            'queued',
            'manifest',
            'download',
            'decrypt',
            'remux',
            'tag',
            'done',
            'failed'
        )
    ),
    bytes_done INTEGER NOT NULL DEFAULT 0,
    bytes_total INTEGER NOT NULL DEFAULT 0,
    error TEXT,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (job_id, track_id)
);

-- Index of everything on disk. `path` is relative to the library root so the
-- library can be moved without rewriting rows.
CREATE TABLE tracks (
    id TEXT PRIMARY KEY,
    album_id TEXT,
    title TEXT NOT NULL,
    artist TEXT,
    album TEXT,
    disc_number INTEGER,
    track_number INTEGER,
    duration_ms INTEGER,
    codec TEXT,
    sample_rate INTEGER,
    bit_depth INTEGER,
    path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    downloaded_at TEXT NOT NULL
);

CREATE INDEX idx_tracks_downloaded_at ON tracks (downloaded_at);

CREATE INDEX idx_tracks_album ON tracks (album_id);
