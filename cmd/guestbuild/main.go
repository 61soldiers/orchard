// Command guestbuild writes the fixed parts of the QEMU guest (kernel, base
// initramfs, empty data disk) that ship next to orchard on hosts that run the
// Apple Music daemon in a virtual machine. It runs in release CI on Linux.
//
// With -initramfs it instead assembles the initramfs Orchard builds at run time
// (the base layer plus the daemon's Android tree), so the guest can be booted by
// hand or in CI without Orchard.
package main

import (
	"flag"
	"fmt"
	"os"

	"orchard/internal/guest"
)

func main() {
	arch := flag.String("arch", "x86_64", "guest architecture: x86_64 or aarch64")
	out := flag.String("out", "guest", "output directory")
	cache := flag.String("cache", os.TempDir()+"/orchard-guest-cache", "where downloaded packages are kept")
	rootfs := flag.String("rootfs", "", "with -initramfs: the daemon's rootfs/ directory (from the wrapper release)")
	base := flag.String("base", "", "with -initramfs: base.cpio.gz from a previous run")
	initramfs := flag.String("initramfs", "", "assemble this initramfs file from -base and -rootfs instead of building the guest")
	flag.Parse()

	if *initramfs != "" {
		if *rootfs == "" || *base == "" {
			fmt.Fprintln(os.Stderr, "guestbuild: -initramfs needs -rootfs and -base")
			os.Exit(2)
		}
		if err := guest.BuildInitramfs(*base, *rootfs, *initramfs); err != nil {
			fmt.Fprintln(os.Stderr, "guestbuild:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", *initramfs)
		return
	}
	if err := guest.BuildBase(*arch, *out, *cache); err != nil {
		fmt.Fprintln(os.Stderr, "guestbuild:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", *out)
}
