//go:build windows

package wrapper

import "syscall"

// hideWindow keeps a console window from flashing up for the VM.
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
