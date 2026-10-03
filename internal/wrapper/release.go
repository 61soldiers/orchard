// Package wrapper provisions and supervises the upstream Apple Music DRM
// daemon (github.com/WorldObservationLog/wrapper).
//
// Orchard ships without the daemon: this package fetches the published release
// for the host architecture on first run, so a fresh container needs nothing
// but network access.
package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const (
	releaseOwner = "WorldObservationLog"
	releaseRepo  = "wrapper"
)

// Asset is a resolved wrapper release download.
type Asset struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// SHA256 is the lowercase hex digest GitHub publishes for the asset. It is
	// empty when the API was unreachable and we fell back to a direct URL.
	SHA256 string `json:"sha256,omitempty"`
}

// archRelease maps a Go architecture onto the upstream release naming. Upstream
// only publishes these two; anything else cannot run the daemon at all.
func archRelease(goarch string) (tag, asset string, err error) {
	switch goarch {
	case "amd64":
		return "wrapper.x86_64.latest", "Wrapper.x86_64.latest.zip", nil
	case "arm64":
		return "wrapper.arm64.latest", "Wrapper.arm64.latest.zip", nil
	default:
		return "", "", fmt.Errorf("wrapper has no build for %s/%s; it requires linux amd64 or arm64", runtime.GOOS, goarch)
	}
}

// DefaultTag is the release tag for the running architecture.
func DefaultTag() (string, error) {
	tag, _, err := archRelease(runtime.GOARCH)
	return tag, err
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		// Digest looks like "sha256:abcdef...". Absent on older releases.
		Digest string `json:"digest"`
	} `json:"assets"`
}

// resolveAsset asks the GitHub API for the release so we learn the publisher's
// digest. The API is rate limited to 60 requests/hour for anonymous callers, so
// a failure here is downgraded to a direct download URL rather than an error.
func (p *Provisioner) resolveAsset(ctx context.Context) (Asset, error) {
	tag := p.opts.Tag
	wantName := ""
	if tag == "" {
		defTag, defAsset, err := archRelease(runtime.GOARCH)
		if err != nil {
			return Asset{}, err
		}
		tag, wantName = defTag, defAsset
	}

	fallback := Asset{
		Tag:    tag,
		Name:   wantName,
		SHA256: p.opts.SHA256,
	}
	if wantName != "" {
		fallback.URL = fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", releaseOwner, releaseRepo, tag, wantName)
	}

	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", releaseOwner, releaseRepo, tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Asset{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "orchard")

	resp, err := p.hc.Do(req)
	if err != nil {
		if fallback.URL == "" {
			return Asset{}, fmt.Errorf("resolve release %s: %w", tag, err)
		}
		return fallback, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if fallback.URL == "" {
			return Asset{}, fmt.Errorf("resolve release %s: github returned %s", tag, resp.Status)
		}
		return fallback, nil
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		if fallback.URL == "" {
			return Asset{}, fmt.Errorf("decode release %s: %w", tag, err)
		}
		return fallback, nil
	}

	for _, a := range rel.Assets {
		if !strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			continue
		}
		if wantName != "" && !strings.EqualFold(a.Name, wantName) {
			continue
		}
		return Asset{
			Tag:    tag,
			Name:   a.Name,
			URL:    a.URL,
			SHA256: strings.TrimPrefix(a.Digest, "sha256:"),
		}, nil
	}

	if fallback.URL == "" {
		return Asset{}, fmt.Errorf("release %s has no zip asset", tag)
	}
	return fallback, nil
}

// httpClient is tuned for a ~50 MB download over a possibly slow link.
func httpClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Minute}
}
