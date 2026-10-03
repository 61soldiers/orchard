// Package config loads Orchard's runtime configuration from flags, with
// environment variables as defaults.
package config

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// minAPIKeyLen is the shortest key we accept. The key is the only thing
// standing between the internet and a live Apple Music session.
const minAPIKeyLen = 24

// Config is the fully resolved server configuration.
type Config struct {
	Addr string

	DataDir    string
	DBPath     string
	LibraryDir string
	// WrapperDir holds the provisioned wrapper binary and its rootfs. It is also
	// the daemon's working directory, because wrapper chroots into "./rootfs".
	WrapperDir string

	// APIKey authenticates every request. Never log it.
	APIKey string

	// WrapperAutoProvision downloads the wrapper release on start when missing.
	WrapperAutoProvision bool
	// WrapperTag pins a release tag; empty selects the host architecture's.
	WrapperTag string
	// WrapperURL downloads the zip from here instead of GitHub.
	WrapperURL string
	// WrapperSHA256 pins the expected archive digest.
	WrapperSHA256 string

	// WrapperHost must stay on loopback: the daemon's four ports are unauthenticated.
	WrapperHost                                 string
	DecryptPort, M3U8Port, AccountPort, KeyPort int
	WrapperDeviceInfo                           string
	WrapperProxy                                string

	// FFmpegPath is the ffmpeg the download pipeline remuxes with. The default
	// finds it on PATH; a host that bundles its own (Android) points it here.
	FFmpegPath string

	LogLevel slog.Level

	// TrustProxyHeaders enables X-Forwarded-For/X-Real-IP parsing. Only turn it
	// on when a reverse proxy you control sets those headers.
	TrustProxyHeaders bool

	ShutdownTimeout time.Duration

	// RateLimit/RateBurst bound requests/sec against Orchard's own /v1 surface.
	// Zero disables limiting.
	RateLimit float64
	RateBurst int

	// AppleRateLimit/AppleRateBurst pace outbound calls to Apple's catalog API,
	// independently of MaxConcurrent in-flight requests. Zero disables limiting.
	AppleRateLimit float64
	AppleRateBurst int
}

// Parse reads flags, falling back to ORCHARD_* environment variables, and
// creates the data directories. The API key is environment-only.
func Parse(args []string, output io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("orchard", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprint(output, "Usage: orchard [flags]\n\nEvery flag can also be set through its ORCHARD_* environment variable.\nORCHARD_API_KEY is required and has no flag equivalent.\n\n")
		fs.PrintDefaults()
	}

	var (
		addr       = fs.String("addr", env("ORCHARD_ADDR", ":8080"), "listen address (ORCHARD_ADDR)")
		dataDir    = fs.String("data-dir", env("ORCHARD_DATA_DIR", "./data"), "directory holding orchard.db, library/ and wrapper/ (ORCHARD_DATA_DIR)")
		logLevel   = fs.String("log-level", env("ORCHARD_LOG_LEVEL", "info"), "debug, info, warn or error (ORCHARD_LOG_LEVEL)")
		trustProxy = fs.Bool("trust-proxy-headers", envBool("ORCHARD_TRUST_PROXY_HEADERS", false), "honour X-Forwarded-For/X-Real-IP; only behind a proxy you control (ORCHARD_TRUST_PROXY_HEADERS)")
		shutdown   = fs.Duration("shutdown-timeout", envDuration("ORCHARD_SHUTDOWN_TIMEOUT", 15*time.Second), "graceful shutdown timeout (ORCHARD_SHUTDOWN_TIMEOUT)")

		rateLimit      = fs.Float64("rate-limit", envFloat("ORCHARD_RATE_LIMIT", 20), "requests/sec allowed against the API per key; 0 disables limiting (ORCHARD_RATE_LIMIT)")
		rateBurst      = fs.Int("rate-limit-burst", envInt("ORCHARD_RATE_LIMIT_BURST", 40), "burst size for -rate-limit (ORCHARD_RATE_LIMIT_BURST)")
		appleRateLimit = fs.Float64("apple-rate-limit", envFloat("ORCHARD_APPLE_RATE_LIMIT", 5), "requests/sec Orchard sends to Apple's catalog API; 0 disables limiting (ORCHARD_APPLE_RATE_LIMIT)")
		appleRateBurst = fs.Int("apple-rate-limit-burst", envInt("ORCHARD_APPLE_RATE_LIMIT_BURST", 10), "burst size for -apple-rate-limit (ORCHARD_APPLE_RATE_LIMIT_BURST)")

		autoProv   = fs.Bool("wrapper-auto-provision", envBool("ORCHARD_WRAPPER_AUTO_PROVISION", true), "download the wrapper release on start when missing (ORCHARD_WRAPPER_AUTO_PROVISION)")
		wrapperTag = fs.String("wrapper-tag", env("ORCHARD_WRAPPER_TAG", ""), "pin a wrapper release tag; empty picks the host architecture's (ORCHARD_WRAPPER_TAG)")
		wrapperURL = fs.String("wrapper-url", env("ORCHARD_WRAPPER_URL", ""), "download the wrapper zip from this URL instead of GitHub (ORCHARD_WRAPPER_URL)")
		wrapperSum = fs.String("wrapper-sha256", env("ORCHARD_WRAPPER_SHA256", ""), "pin the expected wrapper archive digest (ORCHARD_WRAPPER_SHA256)")

		wrapperHost  = fs.String("wrapper-host", env("ORCHARD_WRAPPER_HOST", "127.0.0.1"), "address the wrapper daemon binds; keep it on loopback (ORCHARD_WRAPPER_HOST)")
		decryptPort  = fs.Int("wrapper-decrypt-port", envInt("ORCHARD_WRAPPER_DECRYPT_PORT", 10020), "wrapper sample-decryption port (ORCHARD_WRAPPER_DECRYPT_PORT)")
		m3u8Port     = fs.Int("wrapper-m3u8-port", envInt("ORCHARD_WRAPPER_M3U8_PORT", 20020), "wrapper manifest port (ORCHARD_WRAPPER_M3U8_PORT)")
		accountPort  = fs.Int("wrapper-account-port", envInt("ORCHARD_WRAPPER_ACCOUNT_PORT", 30020), "wrapper account-info port (ORCHARD_WRAPPER_ACCOUNT_PORT)")
		keyPort      = fs.Int("wrapper-key-port", envInt("ORCHARD_WRAPPER_KEY_PORT", 40020), "wrapper key-template port (ORCHARD_WRAPPER_KEY_PORT)")
		ffmpegPath   = fs.String("ffmpeg", env("ORCHARD_FFMPEG", "ffmpeg"), "ffmpeg binary the download pipeline remuxes with (ORCHARD_FFMPEG)")
		deviceInfo   = fs.String("wrapper-device-info", env("ORCHARD_WRAPPER_DEVICE_INFO", ""), "override the wrapper -I device string (ORCHARD_WRAPPER_DEVICE_INFO)")
		wrapperProxy = fs.String("wrapper-proxy", env("ORCHARD_WRAPPER_PROXY", ""), "proxy for the wrapper daemon, e.g. socks5://host:port (ORCHARD_WRAPPER_PROXY)")
	)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// Never a flag: argv is world-readable through /proc/<pid>/cmdline.
	apiKey := strings.TrimSpace(os.Getenv("ORCHARD_API_KEY"))
	if len(apiKey) < minAPIKeyLen {
		return nil, fmt.Errorf("ORCHARD_API_KEY must be set and at least %d characters", minAPIKeyLen)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return nil, fmt.Errorf("-log-level: %w", err)
	}
	if *shutdown <= 0 {
		return nil, fmt.Errorf("-shutdown-timeout must be positive")
	}

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}

	cfg := &Config{
		Addr:              *addr,
		DataDir:           absData,
		DBPath:            filepath.Join(absData, "orchard.db"),
		LibraryDir:        filepath.Join(absData, "library"),
		WrapperDir:        filepath.Join(absData, "wrapper"),
		APIKey:            apiKey,
		LogLevel:          level,
		TrustProxyHeaders: *trustProxy,
		ShutdownTimeout:   *shutdown,

		WrapperAutoProvision: *autoProv,
		WrapperTag:           strings.TrimSpace(*wrapperTag),
		WrapperURL:           strings.TrimSpace(*wrapperURL),
		WrapperSHA256:        strings.TrimSpace(*wrapperSum),

		WrapperHost:       *wrapperHost,
		DecryptPort:       *decryptPort,
		M3U8Port:          *m3u8Port,
		AccountPort:       *accountPort,
		FFmpegPath:        strings.TrimSpace(*ffmpegPath),
		KeyPort:           *keyPort,
		WrapperDeviceInfo: strings.TrimSpace(*deviceInfo),
		WrapperProxy:      strings.TrimSpace(*wrapperProxy),

		RateLimit:      *rateLimit,
		RateBurst:      *rateBurst,
		AppleRateLimit: *appleRateLimit,
		AppleRateBurst: *appleRateBurst,
	}

	for _, p := range []int{cfg.DecryptPort, cfg.M3U8Port, cfg.AccountPort, cfg.KeyPort} {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("wrapper ports must be between 1 and 65535, got %d", p)
		}
	}
	if cfg.RateLimit < 0 {
		return nil, fmt.Errorf("-rate-limit must not be negative")
	}
	if cfg.AppleRateLimit < 0 {
		return nil, fmt.Errorf("-apple-rate-limit must not be negative")
	}

	for _, dir := range []string{cfg.DataDir, cfg.LibraryDir, cfg.WrapperDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	b, err := strconv.ParseBool(env(key, ""))
	if err != nil {
		return def
	}
	return b
}

func envFloat(key string, def float64) float64 {
	f, err := strconv.ParseFloat(env(key, ""), 64)
	if err != nil {
		return def
	}
	return f
}

func envDuration(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(env(key, ""))
	if err != nil {
		return def
	}
	return d
}

func envInt(key string, def int) int {
	n, err := strconv.Atoi(env(key, ""))
	if err != nil {
		return def
	}
	return n
}
