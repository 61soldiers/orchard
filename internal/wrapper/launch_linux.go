//go:build linux && !android

package wrapper

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

// prootEnv names a static proot binary to fall back on when user namespaces
// can't be used (see daemonCommand). The host supplies it; there is no default.
const prootEnv = "ORCHARD_PROOT"

// userNS is the sandbox the stock daemon builds for itself: a user, mount and
// PID namespace in which it can chroot into rootfs/.
func userNS() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		// Unshared rather than cloned, so Go also remounts / as private: a
		// namespace cloned on a host with shared mounts (any systemd machine)
		// would otherwise propagate the daemon's mounts back out to it.
		Unshareflags: syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
}

// ProbeSandbox is what a `__probe-sandbox` child runs, already inside the
// namespaces userNS creates: it does the two things the daemon needs there, a
// mount and a chroot. Creating the namespaces can succeed while those fail
// (Ubuntu 24.04+ strips an unprivileged namespace's capabilities through
// AppArmor), so only trying them tells the truth.
func ProbeSandbox() error {
	dir, err := os.MkdirTemp("", "orchard-probe-")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	return syscall.Chroot(dir)
}

var (
	probeOnce sync.Once
	probeErr  error
)

// userNSUsable reports whether the daemon can sandbox itself with user
// namespaces here, running the probe once per process.
func userNSUsable() error {
	probeOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			probeErr = err
			return
		}
		cmd := exec.Command(self, ProbeArg)
		cmd.SysProcAttr = userNS()
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = fmt.Errorf("%w: %s", err, out)
		}
	})
	return probeErr
}

// runner picks how the daemon is hosted on this machine: user namespaces when
// the kernel lets an ordinary program have them, else proot if the host supplied
// one, else a virtual machine if the host supplied QEMU. ORCHARD_RUNNER forces a
// choice (for testing).
var (
	runnerOnce sync.Once
	runnerKnd  runnerKind
)

func runner() runnerKind {
	runnerOnce.Do(func() {
		switch runnerKind(os.Getenv(runnerEnv)) {
		case runNative, runProot, runQEMU:
			runnerKnd = runnerKind(os.Getenv(runnerEnv))
			return
		}
		switch {
		case userNSUsable() == nil:
			runnerKnd = runNative
		case os.Getenv(prootEnv) != "":
			runnerKnd = runProot
		default:
			if _, err := qemuFromEnv(); err == nil {
				runnerKnd = runQEMU
			} else {
				runnerKnd = runNative // will fail, and CheckSandbox says why
			}
		}
	})
	return runnerKnd
}

// CheckSandbox reports whether the daemon can run on this host, by any of the
// ways above.
func CheckSandbox() error {
	switch runner() {
	case runProot, runQEMU:
		if runner() == runQEMU {
			_, err := qemuFromEnv()
			return err
		}
		return nil
	}
	if err := userNSUsable(); err != nil {
		return fmt.Errorf("this system does not let programs create the sandbox Apple Music needs "+
			"(unprivileged user namespaces are blocked, as on Ubuntu 24.04+ with AppArmor's restriction), "+
			"and no proot or virtual machine is available as a fallback: %w", err)
	}
	return nil
}

// prepareLaunch readies whatever the chosen runner needs before a start.
func prepareLaunch(dir string, _ ProvisionInfo) error {
	if runner() == runQEMU {
		return qemuPrepare(dir)
	}
	return nil
}

// daemonCommand builds the process that runs the Apple session daemon.
//
// On a desktop host the daemon's target environment does not exist, so the
// upstream launcher fakes one: it unshares a user+mount+PID namespace,
// bind-mounts /dev/urandom, mounts a fresh procfs and chroots into rootfs/
// before exec'ing the Android binary inside it. Orchard creates the namespace
// itself, so that works as a non-root user with no container around it, and
// CLONE_NEWPID plus Pdeathsig guarantee the whole tree dies with Orchard.
//
// Some hosts refuse that (Ubuntu 24.04+ with AppArmor's unprivileged-userns
// restriction, hardened kernels); see runner for what happens then.
func daemonCommand(dir string, info ProvisionInfo, args []string) (*exec.Cmd, error) {
	switch runner() {
	case runQEMU:
		cmd, err := qemuCommand(dir, args)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		return cmd, nil
	case runProot:
		return prootCommand(os.Getenv(prootEnv), dir, args), nil
	}
	cmd := exec.Command(info.BinPath, args...)
	// The stock launcher locates rootfs/ from its own argv[0] and silently exits
	// 0 — no output, nothing listening — when that is a long absolute path
	// (found at ~160 characters; Docker's /data/wrapper/wrapper never got there).
	// Upstream's entrypoint runs it as ./wrapper from its directory, so do the same.
	cmd.Args[0] = "./wrapper"
	cmd.Dir = dir // load-bearing: wrapper does chroot("./rootfs")
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.SysProcAttr = userNS()
	return cmd, nil
}

func prootCommand(proot, dir string, args []string) *exec.Cmd {
	cmd := exec.Command(proot, append([]string{
		// A tracee outlives a killed proot unless told otherwise, and an
		// orphaned daemon keeps its ports.
		"--kill-on-exit",
		"-r", filepath.Join(dir, "rootfs") + "/",
		"-b", "/dev:/dev",
		"-b", "/proc:/proc",
		"-b", "/sys:/sys",
		"-w", "/",
		"/system/bin/main",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd
}

// keyPortSupported reports whether the provisioned build accepts -K; see
// wrapperHasKeyPort.
func keyPortSupported(info ProvisionInfo) bool {
	if runner() == runQEMU {
		return true // the x86_64 release always has it, and we can't run its -h here
	}
	return wrapperHasKeyPort(info.BinPath)
}

// platformProvision lets a platform supply the daemon tree without the
// download-and-extract path. Desktop always provisions from the release.
func (p *Provisioner) platformProvision() (handled bool, err error) {
	return false, nil
}
