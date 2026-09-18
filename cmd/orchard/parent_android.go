//go:build android

package main

import (
	"log/slog"
	"os"
	"syscall"
	"time"
)

// watchParent shuts Orchard down once the app that started it is gone.
//
// On Android the host app, not a service manager, owns this process, and an
// app's children outlive it: they are reparented to init and keep running,
// holding the API port and an authenticated daemon. Verified on device —
// `am force-stop` leaves them up, and the next start then fails to bind.
//
// PR_SET_PDEATHSIG doesn't cover it: it fires when the *thread* that forked
// us exits, and which thread that is inside the host's runtime isn't ours to
// control. So watch for the reparenting itself, and leave through the normal
// SIGTERM path so the daemon is stopped cleanly on the way out.
func watchParent() {
	parent := os.Getppid()
	go func() {
		for range time.Tick(2 * time.Second) {
			if os.Getppid() != parent {
				slog.Warn("host app is gone; shutting down", "parent", parent)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			}
		}
	}()
}
