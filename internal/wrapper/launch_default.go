//go:build !android

package wrapper

import (
	"os"
	"os/exec"
	"syscall"
)

// daemonCommand builds the process that runs the Apple session daemon.
//
// On a desktop host the daemon's target environment does not exist, so the
// upstream launcher fakes one: it unshares a user+mount+PID namespace,
// bind-mounts /dev/urandom, mounts a fresh procfs and chroots into rootfs/
// before exec'ing the Android binary inside it. That is the whole reason the
// desktop deployment needs Docker with seccomp and systempaths relaxed.
//
// The published release already sandboxes itself with
// unshare(CLONE_NEWUSER|NEWNS|NEWPID), but a build from wrapper.c would not,
// and would then need CAP_SYS_ADMIN. Creating the namespace here makes either
// build work as a non-root user, and CLONE_NEWPID plus Pdeathsig guarantees
// the whole tree dies with Orchard rather than leaving an authenticated
// daemon listening.
func daemonCommand(dir string, info ProvisionInfo, args []string) *exec.Cmd {
	cmd := exec.Command(info.BinPath, args...)
	cmd.Dir = dir // load-bearing: wrapper does chroot("./rootfs")
	cmd.Env = append(os.Environ(), "HOME="+dir)
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
	return cmd
}

// keyPortSupported reports whether the provisioned build accepts -K; see
// wrapperHasKeyPort.
func keyPortSupported(info ProvisionInfo) bool {
	return wrapperHasKeyPort(info.BinPath)
}

// platformProvision lets a platform supply the daemon tree without the
// download-and-extract path. Desktop always provisions from the release.
func (p *Provisioner) platformProvision() (handled bool, err error) {
	return false, nil
}
