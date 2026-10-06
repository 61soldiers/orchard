//go:build !linux && !windows

package wrapper

import "syscall"

func hideWindow() *syscall.SysProcAttr { return nil }
