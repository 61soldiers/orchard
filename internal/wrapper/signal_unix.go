//go:build unix

package wrapper

import (
	"os"
	"syscall"
)

// interrupt asks the daemon to stop. It installs a SIGINT handler, so this
// arrives; the caller follows up with Kill if it does not.
func interrupt(p *os.Process) { _ = p.Signal(syscall.SIGINT) }
