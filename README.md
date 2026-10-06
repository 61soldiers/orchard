# Orchard

A self-hosted server that signs in to your own Apple Music subscription and puts a small, stable
HTTP API in front of it: catalog browsing, streaming, downloads and your library. A client never has
to speak Apple's private APIs, hold your Apple session, or know anything about DRM.

Orchard builds on [WorldObservationLog's wrapper](https://github.com/WorldObservationLog/wrapper)
(the component that runs Apple's own Android music libraries and holds the signed-in session) and,
for downloads, [zhaarey's apple-music-downloader](https://github.com/zhaarey/apple-music-downloader).
It adds the setup, supervision, API and storage around them.

You need a paid Apple Music subscription. Orchard signs in as you and does the work for you.

## Ways to run it

| | Linux | macOS | Windows |
| --- | --- | --- | --- |
| **[The program](#run-the-program-linux)** (a release download) | ✅ | ✗ on its own¹ | ✗ on its own¹ |
| **[Docker](#run-it-in-docker-any-system)** | ✅ | ✅ | ✅ |
| **Inside another app** that bundles Orchard | ✅ | ✅ | ✅ |

¹ Apple's component is an Android program, so it needs a Linux kernel. On Linux, Orchard hosts it
itself. macOS and Windows have none, so the app that bundles Orchard supplies a small virtual
machine for it (QEMU and a guest image, see [docs/reference.md](docs/reference.md)). The release
archives contain Orchard only. On those systems, use Docker.

Everything below needs your **Apple ID** and password, for the account with the subscription, and a
64-bit Intel, AMD or ARM computer.

## Run the program (Linux)

Download the archive for your processor from the
[latest release](https://github.com/61soldiers/orchard/releases/latest)
(`orchard_<version>_linux_amd64.tar.gz` or `…_arm64.tar.gz`), check it against `checksums.txt`, and
unpack it:

```shell
sha256sum -c --ignore-missing checksums.txt
tar xzf orchard_*_linux_*.tar.gz
cd orchard_*_linux_*
```

Start it with an access key of your choosing (24 or more characters; treat it like a password):

```shell
export ORCHARD_API_KEY=$(openssl rand -base64 48 | tr -d '=+/')
echo "Your access key: $ORCHARD_API_KEY"
./orchard --addr 127.0.0.1:8080 --data-dir ./data
```

The first start downloads Apple's music component (about 50 MB) and checks it against the published
digest. It then listens on `http://127.0.0.1:8080`. (Without `--addr`, Orchard listens on every
network interface, so keep that flag unless you have put a proxy in front of it.)

Sign in from a second terminal (use the same key):

```shell
curl -sS -X POST http://127.0.0.1:8080/v1/apple/login \
  -H "Authorization: Bearer $ORCHARD_API_KEY" -H 'Content-Type: application/json' \
  -d '{"appleId":"you@example.com","password":"your password"}'
```

If the answer says `"state":"awaiting_2fa"`, Apple has sent a **verification code** to your iPhone,
iPad or Mac. Send it within **60 seconds**:

```shell
curl -sS -X POST http://127.0.0.1:8080/v1/apple/2fa \
  -H "Authorization: Bearer $ORCHARD_API_KEY" -H 'Content-Type: application/json' \
  -d '{"code":"123456"}'
```

`"state":"ready"` means you are signed in. Orchard keeps the session in `./data` and will not ask for
your password again unless it expires or Apple requires re-authentication. Keep `./data` and the
access key safe: anyone with the key can reach your Apple Music account.

## Run it in Docker (any system)

You need Docker:

- Linux: Docker Engine with the Compose plugin. [Install guide](https://docs.docker.com/engine/install/)
- macOS: [Docker Desktop](https://www.docker.com/products/docker-desktop/), running (nothing to configure).
- Windows: [Docker Desktop](https://www.docker.com/products/docker-desktop/) with its default
  **WSL2 backend** and **Linux containers** (both are on out of the box).

Open a terminal in the folder where you want Orchard, then:

**Linux / macOS**

```shell
git clone https://github.com/61soldiers/orchard.git
cd orchard
./setup.sh
```

**Windows** (PowerShell)

```powershell
git clone https://github.com/61soldiers/orchard.git
cd orchard
powershell -ExecutionPolicy Bypass -File .\setup.ps1
```

The setup script does everything else: it checks your machine, starts Orchard, downloads the Apple
Music component, and signs you in. `setup.sh` and `setup.ps1` are the same flow, so pick the one for
your system.

It asks two (perhaps three) questions:

1. **Your Apple ID**: the email address for your subscription.
2. **Your password**: sent straight to Apple, never saved by Orchard.

Apple then sends a **verification code** to your iPhone, iPad or Mac. Type it in when asked. Keep a
device nearby: the code stops working after 60 seconds. If you miss it, run `./setup.sh` again.

When it finishes you get a server address (`http://127.0.0.1:8080`) and an **access key**. The key is
also saved in the `.env` file in the `orchard` folder. Keep it safe: anyone who has it can reach your
Apple Music account.

That is the whole setup. Orchard restarts itself after a reboot and will not ask for your Apple
password again (until it expires or Apple requires re-authentication, in which case run the setup
script again).

## If something goes wrong

**Docker: run the setup script again first** (`./setup.sh`, or `.\setup.ps1` on Windows). It is safe
to repeat and fixes most problems.

**"Docker is not running, or your user cannot reach it"** (Linux)
Start Docker. If it is already running, give yourself permission, then log out and back in:
`sudo usermod -aG docker $USER`

**"Docker Desktop is not running"** (macOS / Windows)
Start Docker Desktop and wait for the whale icon to stop animating. On Windows, if setup reports
Windows-containers mode, right-click the tray icon and choose "Switch to Linux containers...".

**"Unprivileged user namespaces are disabled"** (Linux, either way of running it)
Your system blocks something Orchard needs. On Ubuntu 24.04 and later:
`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`
On Debian or an older kernel: `sudo sysctl -w kernel.unprivileged_userns_clone=1`.
In Docker on Ubuntu 24.04+, add `- apparmor=unconfined` to `security_opt` in `compose.yaml`
(see [docs/reference.md](docs/reference.md#container-privileges)).

**"The Apple Music component did not finish installing"** (Docker on macOS / Windows)
On Windows this is almost always the WSL2 backend being off: Docker Desktop → Settings → General,
tick "Use the WSL 2 based engine", apply, and run the setup script again. On macOS, give Docker
Desktop more memory (Settings → Resources) and retry; check `docker compose logs` either way.

**"Sign-in failed: Apple rejected the credentials"**
Check the email and password. Use your normal Apple ID password, not an app-specific one. If you
have not signed in for a while, sign in once at [music.apple.com](https://music.apple.com), then
try again.

**The verification code did not work**
It has to be sent within 60 seconds. Start the sign-in again with your phone unlocked and ready.

**"This machine's processor is not supported"**
Orchard needs a 64-bit Intel, AMD or ARM processor. Older 32-bit machines, and Raspberry Pis
running a 32-bit system, will not work.

## Connecting a client

A client needs the **server address** and the **access key**.

If the client is on the same machine, the address is `http://127.0.0.1:8080`. Send the key as
`Authorization: Bearer <key>` on every request under `/v1`.

If it is on a different device, do not open port 8080 to your network. Anyone on the network could
read the access key. Put a reverse proxy with HTTPS in front of Orchard first, or use a private
network such as [Tailscale](https://tailscale.com).

Configuration options, the full HTTP API, and how the Apple Music component works are in
[docs/reference.md](docs/reference.md).

## Releases

Releases are cut automatically from the commit history: see the
[releases page](https://github.com/61soldiers/orchard/releases) for binaries, notes and checksums.
