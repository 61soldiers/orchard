package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// EventKind is a milestone parsed from the daemon's stderr.
type EventKind string

const (
	// EventNeeds2FA means the daemon is polling <base-dir>/2fa.txt. Upstream
	// gives up after 60 seconds, so the code must be delivered promptly.
	EventNeeds2FA EventKind = "needs_2fa"
	// EventCodeAccepted means the 2FA file was consumed.
	EventCodeAccepted EventKind = "code_accepted"
	// EventCodeTimeout means the 60 second 2FA window elapsed and the daemon quit.
	EventCodeTimeout EventKind = "code_timeout"
	// EventLoginFailed means authentication was rejected.
	EventLoginFailed EventKind = "login_failed"
	// EventAccountCached means credentials were accepted and the session is stored.
	EventAccountCached EventKind = "account_cached"
	// EventListening means the last of the four services is accepting connections.
	EventListening EventKind = "listening"
	// EventExited means the process is gone.
	EventExited EventKind = "exited"
)

// Event is a state change from the supervised daemon.
type Event struct {
	Kind       EventKind
	Storefront string
	Subscribed bool
	Err        error
}

// TwoFAWindow is upstream's hard limit for delivering the 2FA code.
const TwoFAWindow = 60 * time.Second

// Ports are the four local services the daemon exposes.
type Ports struct {
	Decrypt int
	M3U8    int
	Account int
	Key     int
}

// Config configures the supervised daemon.
type Config struct {
	Host       string
	Ports      Ports
	BaseDir    string // path *inside* the chroot, i.e. wrapper's -B
	DeviceInfo string
	Proxy      string
}

// Login carries first-run credentials. They are only ever passed for the login
// run; once the session is on disk the daemon is started without them.
type Login struct {
	AppleID  string
	Password string
}

// Supervisor owns the wrapper process lifecycle.
type Supervisor struct {
	prov    *Provisioner
	cfg     Config
	onEvent func(Event)

	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	running bool

	// keyPort records whether the wrapper binary launched by the most recent
	// Start accepts -K/--key-port. Older arm64 releases (wrapper 1.2.0 and
	// earlier) have no key-template service: they abort argument parsing on an
	// unknown -K and never print "listening key request on". Guarded by mu.
	keyPort bool
}

// NewSupervisor returns a Supervisor. onEvent is called from a background
// goroutine and must not block.
func NewSupervisor(prov *Provisioner, cfg Config, onEvent func(Event)) *Supervisor {
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	return &Supervisor{prov: prov, cfg: cfg, onEvent: onEvent}
}

// Running reports whether a daemon process is alive.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Start launches the daemon, replacing any running instance. Pass login only
// on first setup.
func (s *Supervisor) Start(ctx context.Context, login *Login) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}

	info := s.prov.Info()
	if info.State != ProvisionReady || info.BinPath == "" {
		return fmt.Errorf("wrapper is not installed (state %s)", info.State)
	}

	keyPort := wrapperHasKeyPort(info.BinPath)
	s.mu.Lock()
	s.keyPort = keyPort
	s.mu.Unlock()

	args := []string{
		"-H", s.cfg.Host,
		"-D", strconv.Itoa(s.cfg.Ports.Decrypt),
		"-M", strconv.Itoa(s.cfg.Ports.M3U8),
		"-A", strconv.Itoa(s.cfg.Ports.Account),
	}
	if keyPort {
		args = append(args, "-K", strconv.Itoa(s.cfg.Ports.Key))
	} else {
		slog.Warn("wrapper build has no key-template service (-K); streaming works, "+
			"key-template downloads will not — upgrade the wrapper release",
			"binary", info.Binary)
	}
	if s.cfg.BaseDir != "" {
		args = append(args, "-B", s.cfg.BaseDir)
	}
	if s.cfg.DeviceInfo != "" {
		args = append(args, "-I", s.cfg.DeviceInfo)
	}
	if s.cfg.Proxy != "" {
		args = append(args, "-P", s.cfg.Proxy)
	}
	if login != nil {
		// Upstream accepts credentials only on argv, so they are briefly visible
		// in /proc/<pid>/cmdline. The caller restarts without them as soon as the
		// session is persisted, which is the only mitigation available.
		args = append(args, "-L", login.AppleID+":"+login.Password, "-F")
	}

	cmd := exec.Command(info.BinPath, args...)
	cmd.Dir = s.prov.Dir() // load-bearing: wrapper does chroot("./rootfs")
	cmd.Env = append(os.Environ(), "HOME="+s.prov.Dir())

	// The published release already sandboxes itself with
	// unshare(CLONE_NEWUSER|NEWNS|NEWPID), but a build from wrapper.c would not,
	// and would then need CAP_SYS_ADMIN. Creating the namespace here makes either
	// build work as a non-root user, and CLONE_NEWPID plus Pdeathsig guarantees
	// the whole tree dies with Orchard rather than leaving an authenticated
	// daemon listening.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start wrapper: %w", err)
	}

	done := make(chan struct{})
	s.mu.Lock()
	s.cmd, s.done, s.running = cmd, done, true
	s.mu.Unlock()

	go s.scan(stderr)
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		close(done)
		s.emit(Event{Kind: EventExited, Err: err})
	}()

	slog.Info("wrapper started", "pid", cmd.Process.Pid, "login", login != nil)
	return nil
}

// Stop terminates the daemon and waits for it to exit.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	cmd, done, running := s.cmd, s.done, s.running
	s.mu.Unlock()

	if !running || cmd == nil || cmd.Process == nil {
		return nil
	}

	// The child is PID 1 of its own namespace, so killing it tears down every
	// descendant. It installs a SIGINT handler, so that arrives; SIGKILL from
	// this ancestor namespace always works if it does not.
	pid := cmd.Process.Pid
	_ = syscall.Kill(pid, syscall.SIGINT)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("wrapper did not exit after SIGKILL")
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	s.mu.Lock()
	s.cmd, s.done = nil, nil
	s.mu.Unlock()
	return nil
}

// WaitListening polls the wrapper's services until all accept a connection
// (three for a build without the key-template service, four with it).
func (s *Supervisor) WaitListening(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	s.mu.Lock()
	keyPort := s.keyPort
	s.mu.Unlock()
	ports := []int{s.cfg.Ports.Decrypt, s.cfg.Ports.M3U8, s.cfg.Ports.Account}
	if keyPort {
		ports = append(ports, s.cfg.Ports.Key)
	}

	for {
		if !s.Running() {
			return errors.New("wrapper exited before its services came up")
		}
		allUp := true
		for _, p := range ports {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(p)), time.Second)
			if err != nil {
				allUp = false
				break
			}
			conn.Close()
		}
		if allUp {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for wrapper services")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Supervisor) emit(e Event) {
	if s.onEvent != nil {
		s.onEvent(e)
	}
}

// scan turns the daemon's stderr into events, redacting secrets before logging.
func (s *Supervisor) scan(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	// The key-template service comes up last, so its listen line is the "ready"
	// signal. A wrapper build without it never prints that line; fall back to
	// the account-info service, which both builds bring up after auth succeeds.
	s.mu.Lock()
	readyLine := "listening key request on"
	if !s.keyPort {
		readyLine = "listening account info request on"
	}
	s.mu.Unlock()

	var storefront string
	var subscribed bool

	for sc.Scan() {
		line := sc.Text()
		slog.Debug("wrapper", "line", redact(line))

		switch {
		case strings.Contains(line, "StoreFront ID:"):
			_, v, _ := strings.Cut(line, "StoreFront ID:")
			storefront, _, _ = strings.Cut(strings.TrimSpace(v), "-")

		case strings.Contains(line, "supports offline channel"):
			subscribed = true

		case strings.Contains(line, "Waiting for input..."):
			s.emit(Event{Kind: EventNeeds2FA})

		case strings.Contains(line, "Code file detected"):
			s.emit(Event{Kind: EventCodeAccepted})

		case strings.Contains(line, "Failed to get 2FA Code"):
			s.emit(Event{Kind: EventCodeTimeout})

		case strings.Contains(line, "login failed"):
			s.emit(Event{Kind: EventLoginFailed, Err: errors.New("apple rejected the credentials")})

		case strings.Contains(line, "account info cached successfully"):
			s.emit(Event{Kind: EventAccountCached, Storefront: storefront, Subscribed: subscribed})

		case strings.Contains(line, readyLine):
			s.emit(Event{Kind: EventListening, Storefront: storefront, Subscribed: subscribed})
		}
	}
}

// redact removes the token prefix upstream prints on startup.
func redact(line string) string {
	if strings.Contains(line, "Music-Token") {
		return "[+] Music-Token: <redacted>"
	}
	return line
}

// wrapperHasKeyPort reports whether the wrapper binary accepts -K/--key-port.
// The daemon aborts all argument parsing on an unknown flag ("invalid option
// -- 'K'") and exits, so passing -K to a build that predates the key-template
// service breaks login entirely. --help lists the full option set and exits
// before the daemon enters its sandbox, so it is a safe probe.
func wrapperHasKeyPort(binPath string) bool {
	out, _ := exec.Command(binPath, "-h").CombinedOutput()
	return bytes.Contains(out, []byte("key-port"))
}
