//go:build unix

package main

import (
	"log/slog"
	"os"
	"runtime"
	"syscall"
	"time"
)

// exitWithParentEnv asks Orchard to shut down once whoever started it is gone.
const exitWithParentEnv = "ORCHARD_EXIT_WITH_PARENT"

// watchParent shuts Orchard down once the app that started it is gone.
//
// An app that launches Orchard itself (Elbert's Apple Music plugin; the
// Android host app) owns the process, but an app's children outlive it: they
// are reparented to init and keep running, holding the API port and an
// authenticated daemon. Verified on Android — `am force-stop` leaves them up,
// and the next start then fails to bind. Such a host sets
// ORCHARD_EXIT_WITH_PARENT=1. Android always does this, whatever the
// environment says.
//
// A container or service manager owns the process otherwise, and Orchard is
// expected to outlive whoever launched it (a shell that ran `nohup orchard`).
//
// PR_SET_PDEATHSIG doesn't cover it: it fires when the *thread* that forked
// us exits, and which thread that is inside the host's runtime isn't ours to
// control. So watch for the reparenting itself, and leave through the normal
// SIGTERM path so the daemon is stopped cleanly on the way out.
func watchParent() {
	if runtime.GOOS != "android" && os.Getenv(exitWithParentEnv) != "1" {
		return
	}
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
