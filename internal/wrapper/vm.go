package wrapper

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchard/internal/guest"
)

// The daemon can run three ways. On Linux it is sandboxed with user namespaces,
// or with proot where those are blocked. Everywhere else — macOS and Windows have
// no Linux kernel to sandbox it with — and on a Linux host that allows neither,
// it runs inside a small Linux virtual machine under QEMU. The host app supplies
// QEMU and the guest's fixed parts; Orchard assembles the rest.
type runnerKind string

const (
	runNative runnerKind = "native"
	runProot  runnerKind = "proot"
	runQEMU   runnerKind = "qemu"
	runNone   runnerKind = "none"
)

const (
	// qemuEnv is the QEMU binary (qemu-system-x86_64). "auto" looks it up on PATH.
	qemuEnv = "ORCHARD_QEMU"
	// guestEnv is the directory holding vmlinuz, base.cpio.gz and data.img.gz,
	// as written by cmd/guestbuild.
	guestEnv = "ORCHARD_GUEST_DIR"
	// qemuShareEnv is QEMU's firmware directory (-L), for a bundled QEMU.
	qemuShareEnv = "ORCHARD_QEMU_SHARE"
	// runnerEnv forces a runner: native, proot or qemu.
	runnerEnv = "ORCHARD_RUNNER"
)

// vmDir is where the guest's own state lives: the initramfs built from the
// daemon's tree, the data disk with the Apple login, the login marker. It is
// beside the daemon's directory, not inside it, because installing a new daemon
// replaces that whole directory and must not take the login with it.
func vmDir(wrapperDir string) string { return filepath.Clean(wrapperDir) + "-vm" }

// guestSession is where the Apple login lives inside the guest's data disk.
const guestBaseDir = "/data/data/com.apple.android.music/files"

type qemuConfig struct {
	Bin      string
	GuestDir string
	Share    string
}

// qemuFromEnv returns the VM configuration the host app supplied, or an error
// saying what is missing.
func qemuFromEnv() (qemuConfig, error) {
	c := qemuConfig{
		Bin:      os.Getenv(qemuEnv),
		GuestDir: os.Getenv(guestEnv),
		Share:    os.Getenv(qemuShareEnv),
	}
	if c.Bin == "" || c.GuestDir == "" {
		return c, fmt.Errorf("no virtual machine is configured (%s and %s)", qemuEnv, guestEnv)
	}
	if c.Bin == "auto" {
		p, err := exec.LookPath("qemu-system-x86_64")
		if err != nil {
			return c, errors.New("qemu-system-x86_64 was not found")
		}
		c.Bin = p
	}
	if _, err := os.Stat(c.Bin); err != nil {
		return c, fmt.Errorf("QEMU is missing: %w", err)
	}
	for _, f := range []string{"vmlinuz", "base.cpio.gz", "data.img.gz"} {
		if _, err := os.Stat(filepath.Join(c.GuestDir, f)); err != nil {
			return c, fmt.Errorf("the guest is incomplete: %w", err)
		}
	}
	if a, _ := os.ReadFile(filepath.Join(c.GuestDir, "ARCH")); strings.TrimSpace(string(a)) != "x86_64" {
		return c, errors.New("only an x86_64 guest is supported")
	}
	return c, nil
}

// qemuPrepare readies the guest for a start: the initramfs holding the daemon's
// Android tree (rebuilt when the daemon or the base layer changed), and the data
// disk that keeps the Apple login between runs.
func qemuPrepare(dir string) error {
	c, err := qemuFromEnv()
	if err != nil {
		return err
	}
	work := vmDir(dir)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	if err := guest.InstallDataImage(filepath.Join(c.GuestDir, "data.img.gz"), filepath.Join(work, "data.img")); err != nil {
		return err
	}
	initrd := filepath.Join(work, "initramfs.cpio.gz")
	newer := func(a, b string) bool {
		ai, aerr := os.Stat(a)
		bi, berr := os.Stat(b)
		return aerr == nil && berr == nil && ai.ModTime().After(bi.ModTime())
	}
	if _, err := os.Stat(initrd); err == nil &&
		!newer(filepath.Join(dir, manifestName), initrd) &&
		!newer(filepath.Join(c.GuestDir, "base.cpio.gz"), initrd) {
		return nil
	}
	return guest.BuildInitramfs(filepath.Join(c.GuestDir, "base.cpio.gz"), filepath.Join(dir, "rootfs"), initrd)
}

// qemuCommand builds the QEMU process for a daemon started with args (as the
// supervisor assembled them for a native run).
func qemuCommand(dir string, args []string) (*exec.Cmd, error) {
	c, err := qemuFromEnv()
	if err != nil {
		return nil, err
	}
	work := vmDir(dir)

	// The daemon listens on every interface of the guest, and QEMU forwards its
	// ports to the address the host-side callers use.
	host := "127.0.0.1"
	var forwards []string
	guestArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-H":
			if i+1 < len(args) {
				host = args[i+1]
				i++
			}
			guestArgs = append(guestArgs, "-H", "0.0.0.0")
			continue
		case "-D", "-M", "-A", "-K":
			if i+1 < len(args) {
				if _, err := strconv.Atoi(args[i+1]); err == nil {
					forwards = append(forwards, args[i+1])
				}
			}
		case "-B":
			// The session lives on the guest's own disk, wherever the host
			// thinks it is.
			if i+1 < len(args) {
				guestArgs = append(guestArgs, "-B", guestBaseDir)
				i++
				continue
			}
		}
		guestArgs = append(guestArgs, a)
	}
	for _, a := range guestArgs {
		if strings.ContainsAny(a, "\r\n") {
			return nil, errors.New("an argument for the daemon contains a line break")
		}
	}

	// Arguments travel as a file QEMU hands to the guest, one per line, so a
	// password never appears in the process list.
	argsFile, err := os.CreateTemp(work, "args-*")
	if err != nil {
		return nil, err
	}
	if _, err := argsFile.WriteString(strings.Join(guestArgs, "\n") + "\n"); err != nil {
		argsFile.Close()
		return nil, err
	}
	argsFile.Close()
	_ = os.Chmod(argsFile.Name(), 0o600)
	// QEMU reads it once, at start.
	time.AfterFunc(30*time.Second, func() { _ = os.Remove(argsFile.Name()) })

	// A login from before the guest (a host daemon's, a Docker volume's) is carried
	// into the guest's disk at this boot, once.
	var seed string
	if v := newVMSession(dir); !markerExists(v) {
		seed, err = seedSession(dir)
		if err != nil {
			return nil, fmt.Errorf("carry the existing login into the guest: %w", err)
		}
		if seed != "" {
			time.AfterFunc(60*time.Second, func() { _ = os.Remove(seed) })
			if err := v.MarkLoggedIn(); err != nil {
				return nil, err
			}
		}
	}

	var hostfwd []string
	for _, p := range forwards {
		hostfwd = append(hostfwd, fmt.Sprintf("hostfwd=tcp:%s:%s-:%s", host, p, p))
	}
	netdev := "user,id=net0"
	if len(hostfwd) > 0 {
		netdev += "," + strings.Join(hostfwd, ",")
	}

	cmdline := []string{
		"-machine", "q35",
		"-m", "1024",
		"-smp", "2",
		"-display", "none",
		"-monitor", "none",
		"-nodefaults",
		"-no-user-config",
		"-no-reboot",
		"-kernel", filepath.Join(c.GuestDir, "vmlinuz"),
		"-initrd", filepath.Join(work, "initramfs.cpio.gz"),
		"-append", "console=ttyS0 quiet loglevel=3 panic=-1 rdinit=/init",
		"-drive", "if=none,id=data0,format=raw,file=" + filepath.Join(work, "data.img"),
		"-device", "virtio-blk-pci,drive=data0",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=net0,romfile=",
		// The console is how the daemon's output reaches Orchard and how a 2FA
		// code reaches the daemon.
		"-chardev", "stdio,id=con,signal=off",
		"-serial", "chardev:con",
		"-fw_cfg", "name=opt/orchard/args,file=" + argsFile.Name(),
	}
	if seed != "" {
		cmdline = append(cmdline, "-fw_cfg", "name=opt/orchard/seed,file="+seed)
	}
	cmdline = append(cmdline, accelArgs()...)
	if c.Share != "" {
		cmdline = append(cmdline, "-L", c.Share)
	}
	cmd := exec.Command(c.Bin, cmdline...)
	cmd.Dir = work
	return cmd, nil
}

// guestCPU is the CPU model every accelerator is asked for. The daemon needs
// SSE4.2 and POPCNT (it dies with SIGILL on the plain qemu64 model, which is what
// upstream's Windows launcher uses for a different daemon), and nothing newer;
// Nehalem is the oldest model with them, so it is also the one the most hosts
// can offer to a guest, under KVM, HVF and WHPX as under plain emulation.
const guestCPU = "Nehalem"

// accelArgs picks the fastest accelerator the host OS has and falls back to
// plain emulation, which is slower but works anywhere.
func accelArgs() []string {
	switch runtime.GOOS {
	case "linux":
		return []string{"-accel", "kvm", "-accel", "tcg", "-cpu", guestCPU}
	case "darwin":
		return []string{"-accel", "hvf", "-accel", "tcg", "-cpu", guestCPU}
	case "windows":
		// Software emulation only: the QEMU built for it has no WHPX (the mingw
		// headers it is built with predate it), and TCG is the tested path.
		return []string{"-accel", "tcg", "-cpu", guestCPU}
	}
	return []string{"-accel", "tcg", "-cpu", guestCPU}
}

// ---- the login inside the guest ------------------------------------------------------

// VMSession is the Apple login of a daemon that runs in the guest. Its files are
// on a disk image the host can't read, so the host keeps a marker of its own, and
// a 2FA code goes in over the console.
type VMSession struct {
	dir string

	mu    sync.Mutex
	stdin io.WriteCloser
}

func newVMSession(dir string) *VMSession { return &VMSession{dir: dir} }

func (v *VMSession) marker() string { return filepath.Join(vmDir(v.dir), "logged-in") }

// LoggedIn reports whether a login completed here before, or one is waiting to be
// moved into the guest (see seedSession).
func (v *VMSession) LoggedIn() bool {
	if _, err := os.Stat(v.marker()); err == nil {
		return true
	}
	return fileSessionDir(v.dir) != ""
}

// MarkLoggedIn records that the daemon reached ready with a login.
func (v *VMSession) MarkLoggedIn() error {
	if err := os.MkdirAll(filepath.Dir(v.marker()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(v.marker(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// Send2FA hands the verification code to the waiting daemon: the guest's init
// reads this line off the console and writes the file the daemon polls.
func (v *VMSession) Send2FA(code string) error {
	if strings.ContainsAny(code, "\r\n") {
		return errors.New("invalid code")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stdin == nil {
		return errors.New("the daemon is not running")
	}
	_, err := io.WriteString(v.stdin, "ORCHARD2FA "+code+"\n")
	return err
}

// Forget removes the login: the marker, and the data disk that holds it (a
// fresh one is unpacked on the next start).
func (v *VMSession) Forget() error {
	for _, f := range []string{v.marker(), filepath.Join(vmDir(v.dir), "data.img")} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// heartbeat is how often the host tells the guest it is still there; the guest
// stops itself after far longer than this without hearing it (see init.sh).
const heartbeat = 5 * time.Second

func (v *VMSession) attach(w io.WriteCloser) {
	v.mu.Lock()
	v.stdin = w
	v.mu.Unlock()
	go func() {
		for range time.Tick(heartbeat) {
			v.mu.Lock()
			cur := v.stdin
			var err error
			if cur == w {
				_, err = io.WriteString(w, "ORCHARDPING\n")
			}
			v.mu.Unlock()
			if cur != w || err != nil {
				return
			}
		}
	}()
}

func (v *VMSession) detach() {
	v.mu.Lock()
	if v.stdin != nil {
		_ = v.stdin.Close()
		v.stdin = nil
	}
	v.mu.Unlock()
}

// portOpen reports whether something answers on the address.
func portOpen(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// daemonGoArch is the architecture of the daemon release to fetch: the guest's
// when the daemon runs in one (always x86_64 so far), else the host's.
func daemonGoArch() string {
	if runner() == runQEMU {
		return "amd64"
	}
	return runtime.GOARCH
}

// ---- moving a file session into the guest -----------------------------------------------

// fileSessionDir is the Apple login of a daemon that ran on the host (user
// namespaces, or Docker before that), in the daemon tree's own data directory, or
// "" when there is none. A VM keeps its login on a disk the host can't write to, so
// such a login is carried in once, at the first boot.
func fileSessionDir(wrapperDir string) string {
	base := filepath.Join(wrapperDir, "rootfs", "data", "data", "com.apple.android.music", "files")
	nonEmpty := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
	}
	if !nonEmpty(filepath.Join(base, "STOREFRONT_ID")) {
		return ""
	}
	if nonEmpty(filepath.Join(base, "mpl_db", "kvs.sqlitedb")) || nonEmpty(filepath.Join(base, "kvs.sqlitedb")) {
		return base
	}
	return ""
}

// seedSession packs the file session as a tar for the guest to unpack into its
// disk (init.sh does, only if the disk has no login yet). It returns "" when there
// is nothing to carry in.
func seedSession(wrapperDir string) (string, error) {
	base := fileSessionDir(wrapperDir)
	if base == "" {
		return "", nil
	}
	f, err := os.CreateTemp(vmDir(wrapperDir), "seed-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_ = os.Chmod(f.Name(), 0o600)
	tw := tar.NewWriter(f)
	// Paths are relative to the guest's /data.
	prefix := "data/com.apple.android.music/files"
	err = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		name := prefix
		if rel != "." {
			name = prefix + "/" + filepath.ToSlash(rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			return nil
		}
		h, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		h.Name = name
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if fi.IsDir() {
			h.Name += "/"
			h.Mode = 0o755
		} else {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err == nil {
		err = tw.Close()
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func markerExists(v *VMSession) bool {
	_, err := os.Stat(v.marker())
	return err == nil
}
