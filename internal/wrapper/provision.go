package wrapper

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ProvisionState is the install state of the wrapper tree.
type ProvisionState string

const (
	ProvisionAbsent      ProvisionState = "absent"
	ProvisionInstalling  ProvisionState = "installing"
	ProvisionReady       ProvisionState = "ready"
	ProvisionFailed      ProvisionState = "failed"
	ProvisionUnsupported ProvisionState = "unsupported"
	manifestName                        = ".provision.json"
	maxUncompressedBytes int64          = 512 << 20
)

// binaryNames are tried in order. Upstream's CMake writes both the plain and
// the rootless build to "wrapper", so the published release is already the
// rootless one; the explicit name is only here for locally built trees.
var binaryNames = []string{"wrapper-rootless", "wrapper"}

// Options configures provisioning.
type Options struct {
	// Dir is the install root. It ends up holding the wrapper binary and rootfs/.
	Dir string
	// Tag pins a release tag. Empty selects the one matching the host arch.
	Tag string
	// URL bypasses GitHub entirely and downloads the zip from here.
	URL string
	// SHA256 pins the expected digest. Empty trusts the published digest.
	SHA256 string
}

// ProvisionInfo is the public view of the install.
type ProvisionInfo struct {
	State   ProvisionState `json:"state"`
	Tag     string         `json:"tag,omitempty"`
	Binary  string         `json:"binary,omitempty"`
	SHA256  string         `json:"sha256,omitempty"`
	Error   string         `json:"error,omitempty"`
	Rootfs  string         `json:"-"`
	BinPath string         `json:"-"`
}

type manifest struct {
	Tag         string    `json:"tag"`
	SHA256      string    `json:"sha256"`
	URL         string    `json:"url"`
	Binary      string    `json:"binary"`
	InstalledAt time.Time `json:"installed_at"`
}

// Provisioner installs the wrapper release into a directory, idempotently.
type Provisioner struct {
	opts Options
	hc   *http.Client

	mu   sync.RWMutex
	info ProvisionInfo
}

// NewProvisioner returns a Provisioner for opts.Dir.
func NewProvisioner(opts Options) *Provisioner {
	p := &Provisioner{opts: opts, hc: httpClient()}
	p.info = ProvisionInfo{State: ProvisionAbsent, Rootfs: p.RootfsDir()}
	return p
}

// RootfsDir is the chroot tree wrapper runs against.
func (p *Provisioner) RootfsDir() string { return filepath.Join(p.opts.Dir, "rootfs") }

// Dir is the install root, which is also wrapper's required working directory.
func (p *Provisioner) Dir() string { return p.opts.Dir }

// Info reports the current install state.
func (p *Provisioner) Info() ProvisionInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.info
}

func (p *Provisioner) setInfo(i ProvisionInfo) {
	i.Rootfs = p.RootfsDir()
	p.mu.Lock()
	p.info = i
	p.mu.Unlock()
}

// Detect reports the on-disk install state without touching the network. Use
// it when auto-provisioning is disabled.
func (p *Provisioner) Detect() error {
	m, bin, ok := p.installed()
	if !ok {
		p.setInfo(ProvisionInfo{State: ProvisionAbsent})
		return errors.New("wrapper is not installed and auto-provisioning is disabled")
	}
	p.setInfo(ProvisionInfo{
		State: ProvisionReady, Tag: m.Tag, SHA256: m.SHA256,
		Binary: m.Binary, BinPath: bin,
	})
	return nil
}

// Ensure installs the wrapper release if it is missing or does not match the
// pinned tag/digest. It is safe to call on every start.
func (p *Provisioner) Ensure(ctx context.Context) error {
	if _, _, err := archRelease(runtime.GOARCH); err != nil && p.opts.URL == "" {
		p.setInfo(ProvisionInfo{State: ProvisionUnsupported, Error: err.Error()})
		return err
	}

	if m, bin, ok := p.installed(); ok && p.satisfies(m) {
		p.setInfo(ProvisionInfo{
			State: ProvisionReady, Tag: m.Tag, SHA256: m.SHA256,
			Binary: m.Binary, BinPath: bin,
		})
		slog.Info("wrapper already installed", "tag", m.Tag, "binary", m.Binary)
		return nil
	}

	p.setInfo(ProvisionInfo{State: ProvisionInstalling})

	if err := p.install(ctx); err != nil {
		p.setInfo(ProvisionInfo{State: ProvisionFailed, Error: err.Error()})
		return err
	}

	m, bin, ok := p.installed()
	if !ok {
		err := errors.New("install finished but no wrapper binary was found")
		p.setInfo(ProvisionInfo{State: ProvisionFailed, Error: err.Error()})
		return err
	}
	p.setInfo(ProvisionInfo{
		State: ProvisionReady, Tag: m.Tag, SHA256: m.SHA256,
		Binary: m.Binary, BinPath: bin,
	})
	slog.Info("wrapper installed", "tag", m.Tag, "binary", m.Binary, "sha256", m.SHA256)
	return nil
}

// satisfies reports whether an existing install matches the configured pins.
func (p *Provisioner) satisfies(m manifest) bool {
	if p.opts.SHA256 != "" && !strings.EqualFold(p.opts.SHA256, m.SHA256) {
		return false
	}
	if p.opts.Tag != "" && p.opts.Tag != m.Tag {
		return false
	}
	return true
}

// installed reads the manifest and confirms the tree is actually present.
func (p *Provisioner) installed() (manifest, string, bool) {
	data, err := os.ReadFile(filepath.Join(p.opts.Dir, manifestName))
	if err != nil {
		return manifest{}, "", false
	}
	var m manifest
	if json.Unmarshal(data, &m) != nil || m.Binary == "" {
		return manifest{}, "", false
	}
	bin := filepath.Join(p.opts.Dir, m.Binary)
	if st, err := os.Stat(bin); err != nil || st.IsDir() {
		return manifest{}, "", false
	}
	if st, err := os.Stat(p.RootfsDir()); err != nil || !st.IsDir() {
		return manifest{}, "", false
	}
	return m, bin, true
}

func (p *Provisioner) install(ctx context.Context) error {
	asset := Asset{Tag: p.opts.Tag, URL: p.opts.URL, SHA256: p.opts.SHA256}
	if asset.URL == "" {
		resolved, err := p.resolveAsset(ctx)
		if err != nil {
			return err
		}
		asset = resolved
		if p.opts.SHA256 != "" {
			asset.SHA256 = p.opts.SHA256
		}
	}

	parent := filepath.Dir(p.opts.Dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}

	zipPath, gotDigest, err := p.download(ctx, asset)
	if err != nil {
		return err
	}
	defer os.Remove(zipPath)

	if asset.SHA256 != "" && !strings.EqualFold(asset.SHA256, gotDigest) {
		return fmt.Errorf("wrapper download digest mismatch: want %s, got %s", asset.SHA256, gotDigest)
	}

	stage, err := os.MkdirTemp(parent, ".wrapper-stage-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	if err := unzip(zipPath, stage); err != nil {
		return fmt.Errorf("extract wrapper: %w", err)
	}

	root, binName, err := locateTree(stage)
	if err != nil {
		return err
	}

	if err := os.Chmod(filepath.Join(root, binName), 0o755); err != nil {
		return err
	}

	// Carry the Apple session across upgrades: it lives under rootfs/data and
	// is the only part of the tree that is not reproducible from the release.
	oldData := filepath.Join(p.RootfsDir(), "data")
	if st, err := os.Stat(oldData); err == nil && st.IsDir() {
		newData := filepath.Join(root, "rootfs", "data")
		if err := os.RemoveAll(newData); err != nil {
			return err
		}
		if err := os.Rename(oldData, newData); err != nil {
			return fmt.Errorf("preserve apple session: %w", err)
		}
		slog.Info("preserved existing apple session across wrapper upgrade")
	}

	m := manifest{
		Tag: asset.Tag, SHA256: gotDigest, URL: asset.URL,
		Binary: binName, InstalledAt: time.Now().UTC(),
	}
	buf, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, manifestName), buf, 0o600); err != nil {
		return err
	}

	retired := ""
	if _, err := os.Stat(p.opts.Dir); err == nil {
		retired = p.opts.Dir + ".old"
		_ = os.RemoveAll(retired)
		if err := os.Rename(p.opts.Dir, retired); err != nil {
			return fmt.Errorf("retire previous wrapper install: %w", err)
		}
	}
	if err := os.Rename(root, p.opts.Dir); err != nil {
		if retired != "" {
			_ = os.Rename(retired, p.opts.Dir)
		}
		return fmt.Errorf("activate wrapper install: %w", err)
	}
	if retired != "" {
		_ = os.RemoveAll(retired)
	}
	return nil
}

// download streams the asset to a temp file, hashing as it goes.
func (p *Provisioner) download(ctx context.Context, a Asset) (path, digest string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "orchard")

	slog.Info("downloading wrapper release", "tag", a.Tag, "url", a.URL)
	resp, err := p.hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("download wrapper: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("download wrapper: %s returned %s", a.URL, resp.Status)
	}

	f, err := os.CreateTemp(filepath.Dir(p.opts.Dir), ".wrapper-dl-*.zip")
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxUncompressedBytes)); err != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("download wrapper: %w", err)
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

// locateTree finds the directory holding rootfs/ plus a wrapper binary. The
// release zip has them at the top level, but tolerate one nested folder.
func locateTree(stage string) (root, binary string, err error) {
	candidates := []string{stage}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			candidates = append(candidates, filepath.Join(stage, e.Name()))
		}
	}

	for _, dir := range candidates {
		if st, err := os.Stat(filepath.Join(dir, "rootfs")); err != nil || !st.IsDir() {
			continue
		}
		for _, name := range binaryNames {
			if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
				return dir, name, nil
			}
		}
	}
	return "", "", errors.New("wrapper archive did not contain a rootfs/ directory next to a wrapper binary")
}

// unzip extracts src into dst, rejecting paths that escape dst.
func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	var written int64
	for _, f := range zr.File {
		target, err := safeJoin(dst, f.Name)
		if err != nil {
			return err
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// Symlinks in the tree would be another way out of dst; the release has
		// none, so refuse rather than resolve them.
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("wrapper archive contains a symlink (%s); refusing", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		mode := os.FileMode(0o644)
		if f.Mode()&0o111 != 0 {
			mode = 0o755
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxUncompressedBytes-written))
		rc.Close()
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		written += n
		if written >= maxUncompressedBytes {
			return errors.New("wrapper archive is larger than the 512 MiB limit")
		}
	}
	return nil
}

// safeJoin resolves name under base, rejecting absolute paths and traversal.
func safeJoin(base, name string) (string, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("wrapper archive contains an absolute path (%s)", name)
	}
	target := filepath.Join(base, filepath.FromSlash(name))
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("wrapper archive entry escapes the install directory (%s)", name)
	}
	return target, nil
}
