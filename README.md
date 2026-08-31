# Orchard

Wrapper around WorldObservationLog's wrapper and (partly) zhaarey's apple-music-downloader to easily set it up. Download and stream apple music tracks with ease

You need a paid Apple Music subscription. Orchard signs in as you and does the work for you.

## What you need

- A 64-bit Intel, AMD or ARM computer running **Linux or Windows**.
- **Docker**:
  - Linux — Docker Engine with the Compose plugin. [Install guide](https://docs.docker.com/engine/install/)
  - Windows — [Docker Desktop](https://www.docker.com/products/docker-desktop/) with its default
    **WSL2 backend** and **Linux containers** (both are on out of the box).
- Your **Apple ID** and password, for the account with the subscription.

## Setup

Open a terminal in the folder where you want Orchard, then:

**Linux**

```shell
git clone https://github.com/evolvedmesh/orchard.git
cd orchard
./setup.sh
```

**Windows** (PowerShell)

```powershell
git clone https://github.com/evolvedmesh/orchard.git
cd orchard
powershell -ExecutionPolicy Bypass -File .\setup.ps1
```

The setup script does everything else: it checks your machine, starts Orchard, downloads the Apple
Music component ([wrapper](https://github.com/WorldObservationLog/wrapper)), and signs you in.
`setup.sh` and `setup.ps1` are the same flow — pick the one for your platform.

It asks two (perhaps three) questions:

1. **Your Apple ID** — the email address for your subscription.
2. **Your password** — sent straight to Apple, never saved by Orchard.

Apple then sends a **verification code** to your iPhone, iPad or Mac. Type it in when asked. Keep a
device nearby: the code stops working after 60 seconds. If you miss it, run `./setup.sh` again.

When it finishes:

```text
  ✓ Signed in

All set

  Server address:  http://127.0.0.1:8080
  Access key:      kTdN9pQ2xRv7…
```

**Keep the access key safe.** Your music player needs it, and anyone who has it can reach your
Apple Music account. It is also saved in the `.env` file in the `orchard` folder.

That is the whole setup. Orchard restarts itself after a reboot and will not ask for your Apple
password again (until it expires or Apple requires re-authentication, in which case run setup.sh again).

## If something goes wrong

**Run the setup script again first** (`./setup.sh`, or `.\setup.ps1` on Windows). It is safe to
repeat and fixes most problems.

**"Docker is not running, or your user cannot reach it"** (Linux)
Start Docker. If it is already running, give yourself permission, then log out and back in:
`sudo usermod -aG docker $USER`

**"Docker Desktop is not running" / "is in Windows-containers mode"** (Windows)
Start Docker Desktop and wait for the whale icon to stop animating. If setup reports
Windows-containers mode, right-click the tray icon and choose "Switch to Linux containers...".

**"Unprivileged user namespaces are disabled"** (Linux)
Your system blocks something Orchard needs. On Ubuntu 24.04 and later:
`sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`

**"The Apple Music component did not finish installing"** (Windows)
Almost always the WSL2 backend being off. In Docker Desktop → Settings → General, tick
"Use the WSL 2 based engine", apply, and run `.\setup.ps1` again.

**"Sign-in failed: Apple rejected the credentials"**
Check the email and password. Use your normal Apple ID password, not an app-specific one. If you
have not signed in for a while, sign in once at [music.apple.com](https://music.apple.com), then
try again.

**The verification code did not work**
It has to be typed within 60 seconds. Run `./setup.sh` again with your phone unlocked and ready.

**"This machine's processor is not supported"**
Orchard needs a 64-bit Intel, AMD or ARM processor. Older 32-bit machines, and Raspberry Pis
running a 32-bit system, will not work.

## Using Orchard / Connecting a music player

Your player needs the **server address** and the **access key** that setup printed.

If the player is on the same machine, the address is `http://127.0.0.1:8080`.

If it is on a different device, do not open port 8080 to your network. Anyone on the network could
read the access key. Put a reverse proxy with HTTPS in front of Orchard first, or use a private
network such as [Tailscale](https://tailscale.com).

[elbert](https://github.com/evolvedmesh/elbert) comes with orchard support built in.

Configuration options, the HTTP API, and how the Apple Music component works are in
[docs/reference.md](docs/reference.md).
