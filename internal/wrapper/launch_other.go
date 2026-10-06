//go:build !linux

package wrapper

import (
	"errors"
	"os/exec"
)

var errNoRunner = errors.New("Apple's daemon can only run on Linux, and no virtual machine is set up here to host it " +
	"(the host app has to supply QEMU and the guest image)")

// runner is the virtual machine wherever it is configured: this platform has no
// Linux kernel to sandbox the daemon with.
func runner() runnerKind {
	if _, err := qemuFromEnv(); err == nil {
		return runQEMU
	}
	return runNone
}

// CheckSandbox reports whether the daemon can be hosted here.
func CheckSandbox() error {
	_, err := qemuFromEnv()
	if err != nil {
		return errors.Join(errNoRunner, err)
	}
	return nil
}

// ProbeSandbox is never called off Linux.
func ProbeSandbox() error { return errors.New("not supported on this platform") }

func prepareLaunch(dir string, _ ProvisionInfo) error {
	if runner() == runQEMU {
		return qemuPrepare(dir)
	}
	return errNoRunner
}

func daemonCommand(dir string, _ ProvisionInfo, args []string) (*exec.Cmd, error) {
	if runner() != runQEMU {
		return nil, errNoRunner
	}
	cmd, err := qemuCommand(dir, args)
	if err != nil {
		return nil, err
	}
	// No Pdeathsig here: a QEMU left behind by a killed Orchard stops itself
	// when the heartbeat does (see guest/init.sh).
	cmd.SysProcAttr = hideWindow()
	return cmd, nil
}

// keyPortSupported: the guest runs the x86_64 release, which always has -K, and
// the daemon binary can't be run on this host to ask.
func keyPortSupported(ProvisionInfo) bool { return true }

// platformProvision downloads the daemon like anywhere else when a virtual
// machine is there to run it, and refuses early when there isn't.
func (p *Provisioner) platformProvision() (bool, error) {
	if runner() == runQEMU {
		return false, nil
	}
	p.setInfo(ProvisionInfo{State: ProvisionUnsupported, Error: errNoRunner.Error()})
	return true, errNoRunner
}
