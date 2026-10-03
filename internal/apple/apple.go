// Package apple owns the Apple Music session that the wrapper daemon holds.
//
// It drives the daemon through first-run login (including the 2FA handshake)
// and keeps it running unattended afterwards. The Apple password is used once,
// in memory, and never persisted by Orchard: after a successful login the
// session lives in the wrapper rootfs and the daemon restarts without it.
package apple

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchard/internal/wrapper"
)

// State is the lifecycle of the Apple session.
type State string

const (
	StateUnconfigured State = "unconfigured"
	StateStarting     State = "starting"
	StateAwaiting2FA  State = "awaiting_2fa"
	StateReady        State = "ready"
	StateFailed       State = "failed"
)

// baseDirRel is the wrapper's -B directory, relative to the rootfs root.
const baseDirRel = "data/data/com.apple.android.music/files"

// ChrootBaseDir is the same directory as seen from inside the chroot.
const ChrootBaseDir = "/" + baseDirRel

// twoFAFile is polled by the daemon during an HSA2 login. Upstream reads it
// with fscanf("%6s"), deletes it, and gives up after 60 seconds.
const twoFAFile = "2fa.txt"

// readyTimeout bounds how long we wait for the four services after login.
const readyTimeout = 90 * time.Second

// Status is the public view of the session. It never carries tokens.
type Status struct {
	State      State                 `json:"state"`
	Storefront string                `json:"storefront,omitempty"`
	Subscribed bool                  `json:"subscribed"`
	Error      string                `json:"error,omitempty"`
	Wrapper    wrapper.ProvisionInfo `json:"wrapper"`
}

// ErrBusy is returned when a login is already in flight.
var ErrBusy = errors.New("an apple login is already in progress")

// ErrNot2FA is returned when a code arrives outside the 2FA window.
var ErrNot2FA = errors.New("no 2FA code is being requested")

// Manager drives the wrapper's Apple session.
type Manager struct {
	prov *wrapper.Provisioner
	sup  *wrapper.Supervisor
	cfg  wrapper.Config
	hc   *http.Client

	mu         sync.Mutex
	state      State
	storefront string
	subscribed bool
	lastErr    string
	changed    chan struct{}

	// loginActive guards the login/2FA sequence against concurrent callers.
	loginActive bool

	accountState
}

// NewManager wires a Manager to the provisioner it supervises.
func NewManager(prov *wrapper.Provisioner, cfg wrapper.Config) *Manager {
	if cfg.BaseDir == "" {
		cfg.BaseDir = ChrootBaseDir
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	m := &Manager{
		prov:    prov,
		cfg:     cfg,
		hc:      &http.Client{Timeout: 15 * time.Second},
		state:   StateUnconfigured,
		changed: make(chan struct{}),
	}
	m.sup = wrapper.NewSupervisor(prov, cfg, m.handleEvent)
	return m
}

// BaseDir is the wrapper's -B path on the host side of the chroot.
func (m *Manager) BaseDir() string {
	return filepath.Join(m.prov.RootfsDir(), baseDirRel)
}

// sessionDBRelPaths are the kvs store's location relative to BaseDir, most
// preferred first. Newer wrapper builds keep it in mpl_db/; wrapper 1.2.0 and
// earlier (notably the arm64 release) write it directly under the -B directory.
var sessionDBRelPaths = []string{
	filepath.Join("mpl_db", "kvs.sqlitedb"),
	"kvs.sqlitedb",
}

// locateSessionDB returns the first existing, non-empty kvs store under baseDir
// and true, or the preferred path and false when none is present yet.
func locateSessionDB(baseDir string) (string, bool) {
	for _, rel := range sessionDBRelPaths {
		p := filepath.Join(baseDir, rel)
		if nonEmptyFile(p) {
			return p, true
		}
	}
	return filepath.Join(baseDir, sessionDBRelPaths[0]), false
}

// SessionDBPath is the key-value store the daemon writes after a successful
// login. Its existence is what upstream uses to decide whether -L is needed.
func (m *Manager) SessionDBPath() string {
	p, _ := locateSessionDB(m.BaseDir())
	return p
}

// LoggedIn reports whether a usable Apple session exists on disk.
//
// The session database alone is not proof: the daemon creates it while starting
// up, before authentication is attempted, so a rejected login leaves one
// behind. STOREFRONT_ID is deleted at the start of every login and rewritten
// only once the account has been cached, so require both.
func (m *Manager) LoggedIn() bool {
	_, ok := locateSessionDB(m.BaseDir())
	return ok && nonEmptyFile(filepath.Join(m.BaseDir(), "STOREFRONT_ID"))
}

func nonEmptyFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// Status describes the current session.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		State:      m.state,
		Storefront: m.storefront,
		Subscribed: m.subscribed,
		Error:      m.lastErr,
		Wrapper:    m.prov.Info(),
	}
}

// setState records a transition and wakes every waiter.
func (m *Manager) setState(s State, errMsg string) {
	m.mu.Lock()
	m.state, m.lastErr = s, errMsg
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
	slog.Info("apple session state", "state", s, "error", errMsg)
}

func (m *Manager) handleEvent(e wrapper.Event) {
	switch e.Kind {
	case wrapper.EventNeeds2FA:
		m.setState(StateAwaiting2FA, "")

	case wrapper.EventCodeAccepted:
		m.setState(StateStarting, "")

	case wrapper.EventCodeTimeout:
		m.setState(StateFailed, "2FA code was not supplied within 60 seconds")

	case wrapper.EventLoginFailed:
		m.setState(StateFailed, "Apple rejected the credentials")

	case wrapper.EventAccountCached, wrapper.EventListening:
		m.mu.Lock()
		if e.Storefront != "" {
			m.storefront = e.Storefront
		}
		m.subscribed = m.subscribed || e.Subscribed
		m.mu.Unlock()
		if e.Kind == wrapper.EventListening {
			m.setState(StateReady, "")
		}

	case wrapper.EventExited:
		m.mu.Lock()
		wasReady := m.state == StateReady
		m.mu.Unlock()
		if wasReady {
			// The persisted session (LoggedIn) survives a daemon crash — only the
			// in-memory process is gone — so this is recoverable without asking
			// the user to sign in again. go recoverAfterCrash instead of failing
			// outright; it falls back to StateFailed itself if the daemon won't
			// come back up.
			m.setState(StateStarting, "the wrapper daemon exited unexpectedly, reconnecting")
			go m.recoverAfterCrash()
		}
	}
}

// crashRestartAttempts bounds recoverAfterCrash: a daemon that won't come back
// after this many tries has a real problem retrying won't fix.
const crashRestartAttempts = 3

// crashRestartBackoff is the pause between recoverAfterCrash attempts.
const crashRestartBackoff = 5 * time.Second

// recoverAfterCrash runs in its own goroutine after the wrapper daemon exits
// while the session was ready. It is not a login: the daemon is simply
// restarted against the session already on disk, the same thing Autostart
// does at process boot, so a transient crash resolves itself instead of
// leaving the user staring at a sign-in form for a session that was never
// actually lost.
func (m *Manager) recoverAfterCrash() {
	if !m.LoggedIn() {
		return
	}

	var lastErr error
	for attempt := 1; attempt <= crashRestartAttempts; attempt++ {
		m.mu.Lock()
		loginActive := m.loginActive
		m.mu.Unlock()
		if loginActive {
			// A real Login()/Submit2FA() call now owns the state machine;
			// stepping on it here would race the daemon it just started.
			return
		}

		ctx := context.Background()
		if err := m.sup.Start(ctx, nil); err != nil {
			lastErr = err
		} else if err := m.sup.WaitListening(ctx, readyTimeout); err != nil {
			lastErr = err
		} else {
			m.setState(StateReady, "")
			slog.Info("wrapper daemon recovered after unexpected exit", "attempt", attempt)
			return
		}

		if attempt < crashRestartAttempts {
			time.Sleep(crashRestartBackoff)
		}
	}

	m.setState(StateFailed, fmt.Sprintf(
		"the wrapper daemon exited unexpectedly and could not be restarted: %v", lastErr))
}

// Autostart brings the daemon up when a session already exists. It is a no-op
// when Apple has never been configured.
func (m *Manager) Autostart(ctx context.Context) error {
	if !m.LoggedIn() {
		m.setState(StateUnconfigured, "")
		return nil
	}
	m.setState(StateStarting, "")

	if err := m.sup.Start(ctx, nil); err != nil {
		m.setState(StateFailed, err.Error())
		return err
	}
	if err := m.sup.WaitListening(ctx, readyTimeout); err != nil {
		m.setState(StateFailed, err.Error())
		return err
	}
	m.setState(StateReady, "")
	return nil
}

// Login starts the daemon with credentials. It returns once the session is
// ready or the daemon asks for a 2FA code, whichever comes first.
func (m *Manager) Login(ctx context.Context, appleID, password string) (Status, error) {
	m.mu.Lock()
	if m.loginActive {
		m.mu.Unlock()
		return m.Status(), ErrBusy
	}
	m.loginActive = true
	m.storefront, m.subscribed = "", false
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.loginActive = false
		m.mu.Unlock()
	}()

	if info := m.prov.Info(); info.State != wrapper.ProvisionReady {
		return m.Status(), fmt.Errorf("wrapper is not installed yet (state %s)", info.State)
	}

	// A stale 2fa.txt would be consumed by the new run before the user ever
	// sees a prompt, so clear it first.
	_ = os.Remove(filepath.Join(m.BaseDir(), twoFAFile))

	m.setState(StateStarting, "")
	if err := m.sup.Start(ctx, &wrapper.Login{AppleID: appleID, Password: password}); err != nil {
		m.setState(StateFailed, err.Error())
		return m.Status(), err
	}

	st, err := m.waitFor(ctx, readyTimeout, StateAwaiting2FA, StateReady, StateFailed)
	if err != nil {
		return m.Status(), err
	}
	if st == StateReady {
		return m.finishLogin(ctx)
	}
	return m.Status(), nil
}

// Submit2FA hands the verification code to the waiting daemon.
func (m *Manager) Submit2FA(ctx context.Context, code string) (Status, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return m.Status(), errors.New("code is required")
	}
	// Upstream parses with %6s; anything longer would be silently truncated.
	if len(code) > 6 {
		return m.Status(), errors.New("code must be at most 6 characters")
	}

	m.mu.Lock()
	state := m.state
	m.mu.Unlock()
	if state != StateAwaiting2FA {
		return m.Status(), ErrNot2FA
	}

	dir := m.BaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return m.Status(), err
	}
	// No trailing newline: upstream reads exactly six characters.
	if err := os.WriteFile(filepath.Join(dir, twoFAFile), []byte(code), 0o600); err != nil {
		return m.Status(), fmt.Errorf("write 2FA code: %w", err)
	}

	st, err := m.waitFor(ctx, readyTimeout, StateReady, StateFailed)
	if err != nil {
		return m.Status(), err
	}
	if st == StateReady {
		return m.finishLogin(ctx)
	}
	return m.Status(), nil
}

// finishLogin restarts the daemon without credentials. That drops the password
// from the process argv and proves the persisted session works unattended,
// which is what every later restart relies on.
func (m *Manager) finishLogin(ctx context.Context) (Status, error) {
	if !m.LoggedIn() {
		m.setState(StateFailed, "login reported success but no session was written")
		return m.Status(), errors.New("no session was written")
	}

	slog.Info("restarting wrapper without credentials")
	m.setState(StateStarting, "")

	if err := m.sup.Start(ctx, nil); err != nil {
		m.setState(StateFailed, err.Error())
		return m.Status(), err
	}
	if err := m.sup.WaitListening(ctx, readyTimeout); err != nil {
		m.setState(StateFailed, err.Error())
		return m.Status(), err
	}
	m.setState(StateReady, "")
	return m.Status(), nil
}

// Logout stops the daemon and removes the persisted Apple session.
func (m *Manager) Logout(ctx context.Context) error {
	if err := m.sup.Stop(ctx); err != nil {
		return err
	}
	base := m.BaseDir()
	// mpl_db/ and a bare kvs.sqlitedb (+ its -wal/-shm) cover both wrapper
	// session layouts; STOREFRONT_ID is the file LoggedIn keys on.
	stale := []string{
		"mpl_db", "STOREFRONT_ID", "MUSIC_TOKEN", twoFAFile,
		"kvs.sqlitedb", "kvs.sqlitedb-wal", "kvs.sqlitedb-shm",
	}
	for _, name := range stale {
		if err := os.RemoveAll(filepath.Join(base, name)); err != nil {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	m.mu.Lock()
	m.storefront, m.subscribed = "", false
	m.mu.Unlock()
	m.setState(StateUnconfigured, "")
	return nil
}

// Shutdown stops the daemon.
func (m *Manager) Shutdown(ctx context.Context) error { return m.sup.Stop(ctx) }

// waitFor blocks until the session reaches one of want, or timeout elapses.
func (m *Manager) waitFor(ctx context.Context, timeout time.Duration, want ...State) (State, error) {
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		cur, ch := m.state, m.changed
		m.mu.Unlock()

		for _, w := range want {
			if cur == w {
				return cur, nil
			}
		}

		select {
		case <-ch:
		case <-deadline:
			return cur, fmt.Errorf("timed out waiting for the apple session (still %s)", cur)
		case <-ctx.Done():
			return cur, ctx.Err()
		}
	}
}
