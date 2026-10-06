//go:build android

package wrapper

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// On Android the daemon's target environment *is* the host. rootfs/system/bin/main
// is an Android NDK executable (API 22, aarch64) linking Apple's own
// libandroidappmusic/libstoreservicescore/libmediaplatform, so the ABI needs no
// faking — which is why none of the desktop Docker apparatus applies here.
//
// What still has to be supplied is path isolation. The daemon resolves its
// libraries through /system/lib64 and its interpreter through /system/bin/linker64,
// and those must reach the bundled Android-5-era tree rather than the device's
// own modern ones; mixing the two segfaults the process immediately. An app has
// neither CAP_SYS_CHROOT nor unprivileged user namespaces, so proot supplies that
// isolation with ptrace instead of a chroot.
//
// Two Android rules shape the rest:
//
//   - Only files in the APK's nativeLibraryDir may be executed or mapped
//     executable (SELinux W^X for targetSdk >= 29). So the whole tree ships as
//     native libraries and is bound into place, never extracted under dir.
//   - proot normally extracts its own loader to a temp dir and execs it, which
//     that same rule forbids. PROOT_LOADER points it at the loader shipped as a
//     native library instead.
//
// The host app passes the two paths only it knows: its nativeLibraryDir
// (ORCHARD_ANDROID_LIB_DIR) and a writable cache dir (ORCHARD_ANDROID_TMP_DIR).

// CheckSandbox is always fine on Android: proot supplies the isolation.
func CheckSandbox() error { return nil }

// runner is native: the daemon's own ABI is the host's, with proot for paths.
func runner() runnerKind { return runNative }

// prepareLaunch has nothing to do: the tree ships in the APK.
func prepareLaunch(string, ProvisionInfo) error { return nil }

// ProbeSandbox is never called on Android: proot supplies the isolation.
func ProbeSandbox() error { return nil }

const (
	androidLibDirEnv = "ORCHARD_ANDROID_LIB_DIR"
	androidTmpDirEnv = "ORCHARD_ANDROID_TMP_DIR"

	// The daemon tree as the host app packages it. Everything in
	// nativeLibraryDir must be named lib*.so to be extracted at install, so the
	// two executables and proot's pieces are renamed; the Android-5 platform
	// and Apple libraries keep their own names and are bound over /system/lib64
	// as a whole directory.
	androidMain       = "libmain.so"        // rootfs/system/bin/main
	androidLinker     = "liblinker64.so"    // rootfs/system/bin/linker64
	androidProot      = "libproot.so"       // android/proot
	androidLoader     = "libprootloader.so" // proot's embedded loader, carved out
	androidNetdClient = "libnetdclientnew.so"
)

// sessionDirRel matches apple's baseDirRel: the daemon's -B inside the guest.
// It is the only real directory tree the daemon writes; its own /system comes
// entirely from binds.
const sessionDirRel = "data/data/com.apple.android.music/files"

func daemonCommand(dir string, info ProvisionInfo, args []string) (*exec.Cmd, error) {
	libDir := os.Getenv(androidLibDirEnv)
	tmpDir := os.Getenv(androidTmpDirEnv)
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}

	prootArgs := []string{
		// A tracee outlives a killed proot unless told otherwise, and an orphaned
		// daemon keeps its ports: the next start then fails to bind them.
		"--kill-on-exit",
		"-r", filepath.Join(dir, "rootfs") + "/",
		"-b", "/dev:/dev",
		"-b", "/proc:/proc",
		"-b", "/sys:/sys",
		"-b", libDir + ":/system/lib64",
		"-b", filepath.Join(libDir, androidMain) + ":/system/bin/main",
		"-b", filepath.Join(libDir, androidLinker) + ":/system/bin/linker64",
		// The rootfs's own libnetd_client talks an older netd protocol than the
		// device's; upstream ships a current one for exactly this bind.
		"-b", filepath.Join(libDir, androidNetdClient) + ":/system/lib64/libnetd_client.so",
		"-w", "/",
		// The guest command is the daemon itself, never "linker64 main": its
		// PT_INTERP already names /system/bin/linker64, which inside the guest is
		// the bundled linker. Naming the linker explicitly segfaults.
		"/system/bin/main",
	}

	cmd := exec.Command(filepath.Join(libDir, androidProot), append(prootArgs, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"HOME="+dir,
		"PROOT_TMP_DIR="+tmpDir,
		"PROOT_LOADER="+filepath.Join(libDir, androidLoader),
	)
	// No namespace flags: proot does the isolation, and an app may not unshare.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	return cmd, nil
}

// keyPortSupported is false: the arm64 release's main has no key-template
// service and aborts on -K ("invalid option -- K"), which kills the daemon
// before any of its other services come up. Verified on device.
//
// The desktop path probes --help instead, but here that would mean starting
// the daemon under proot just to read it, and the tree is fixed when the APK is
// built. If a future arm64 release gains -K, the script that bundles the tree
// is the place to detect it.
func keyPortSupported(ProvisionInfo) bool { return false }

// platformProvision takes over provisioning entirely: the daemon tree ships
// inside the APK, so there is nothing to download, verify or upgrade — and
// nothing *could* be, since a downloaded file can never be executed (see the
// W^X note above). All that is left is the directory skeleton proot binds
// into, plus the session directory the daemon writes the Apple login to.
func (p *Provisioner) platformProvision() (bool, error) {
	fail := func(err error) (bool, error) {
		p.setInfo(ProvisionInfo{State: ProvisionFailed, Error: err.Error()})
		return true, err
	}

	libDir := os.Getenv(androidLibDirEnv)
	if libDir == "" {
		return fail(fmt.Errorf("%s is not set; the host app must pass its nativeLibraryDir", androidLibDirEnv))
	}
	for _, name := range []string{androidMain, androidLinker, androidProot, androidLoader, androidNetdClient} {
		if st, err := os.Stat(filepath.Join(libDir, name)); err != nil || st.IsDir() {
			return fail(errors.New("this build does not include the Apple Music daemon (missing " + name + ")"))
		}
	}

	root := p.RootfsDir()
	for _, d := range []string{"system/bin", "system/lib64", "dev", "proc", "sys", sessionDirRel} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return fail(fmt.Errorf("prepare daemon tree: %w", err))
		}
	}

	p.setInfo(ProvisionInfo{
		State:   ProvisionReady,
		Tag:     "bundled",
		Binary:  androidMain,
		BinPath: filepath.Join(libDir, androidMain),
	})
	return true, nil
}
