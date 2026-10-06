//go:build !unix

package wrapper

import "os"

func interrupt(p *os.Process) { _ = p.Kill() }
