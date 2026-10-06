#!/bin/busybox sh
# PID 1 of the guest Orchard boots in QEMU. The Android root filesystem is this
# initramfs itself (system/, with the daemon at /system/bin/main), so there is
# nothing to chroot into: bring up the machine, then run the daemon.
export PATH=/bin:/sbin
/bin/busybox --install -s /bin

mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
# The daemon's stderr is what Orchard reads (login prompts, 2FA, "listening"),
# and QEMU hands the console to it.
exec >/dev/console 2>&1 </dev/console

for m in $(cat /modules.list); do
	insmod "/lib/modules/$m" || echo "[guest] could not load $m"
done

# QEMU's user-mode network: the guest is always 10.0.2.15, the host 10.0.2.2.
ip link set lo up
ip link set eth0 up
ip addr add 10.0.2.15/24 dev eth0
ip route add default via 10.0.2.2
mkdir -p /etc
echo "nameserver 10.0.2.3" >/etc/resolv.conf

# The Apple session lives on a disk image the host keeps between runs.
i=0
while [ ! -b /dev/vda ] && [ "$i" -lt 30 ]; do
	sleep 1
	i=$((i + 1))
done
mkdir -p /data
mount -t ext4 /dev/vda /data || echo "[guest] could not mount the data disk"
mkdir -p /data/data/com.apple.android.music/files

# A login from before this guest existed comes in once, as a tar the host passes the
# same way as the arguments, and only if the disk has none.
SEED=/sys/firmware/qemu_fw_cfg/by_name/opt/orchard/seed/raw
if [ -r "$SEED" ] && [ ! -s /data/data/com.apple.android.music/files/STOREFRONT_ID ]; then
	tar -x -C /data -f "$SEED" || echo "[guest] could not unpack the carried-over login"
fi

# The host passes the daemon's arguments, one per line, as a fw_cfg file rather
# than on the command line, where a password would sit in the process list.
ARGS=/sys/firmware/qemu_fw_cfg/by_name/opt/orchard/args/raw
set --
if [ -r "$ARGS" ]; then
	while IFS= read -r a || [ -n "$a" ]; do
		set -- "$@" "$a"
	done <"$ARGS"
fi

# The release archive ships the Android linker without its execute bit; the stock
# launcher sets it at start, and so must we, or exec of the daemon is refused.
chmod 755 /system/bin/main /system/bin/linker64

export ANDROID_ROOT=/system ANDROID_DATA=/data

# The console is also the host's way to talk to us. A serial line would echo
# what it is sent and turn newlines into CRLF; the host wants neither.
stty -echo -onlcr 2>/dev/null

# A 2FA code arrives as a line "ORCHARD2FA <code>"; the daemon polls 2fa.txt.
# The host also sends a line every few seconds. A serial line can't say that the
# other end is gone, so silence is how we find out, and then we stop rather than
# leave a signed-in daemon running with nobody to stop it.
(
	while true; do
		IFS= read -r -t 40 l
		rc=$?
		if [ "$rc" -ne 0 ]; then
			echo "[guest] console read ended (status $rc)"
			break
		fi
		case "$l" in
		"ORCHARD2FA "*) printf %s "${l#ORCHARD2FA }" >/data/data/com.apple.android.music/files/2fa.txt ;;
		esac
	done
	echo "[guest] host went away"
	sync
	poweroff -f
) </dev/console &
# (An explicit redirect: a background job in a shell without job control gets
# /dev/null for input, which is end-of-file at once.)

/system/bin/main "$@" </dev/null &
wait $!
echo "[guest] daemon exited with status $?"
sync
poweroff -f
