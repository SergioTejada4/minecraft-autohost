go build -o autohost-daemon ./daemon
gradle compileJava
gradle build
go build -trimpath -ldflags '-s -w' -o autohost-daemon-linux-armv7 ./daemon
# AutoHost P2P

Play a Minecraft 1.21.1 world through a Raspberry Pi without keeping a dedicated server running. The first player to join becomes the host; other players join through the same Multiplayer server entry.

The Raspberry Pi runs a small Go daemon for connection routing, world storage, and the web panel. Each player runs Minecraft with the NeoForge client mod. The Pi does not run Minecraft or Java.

- Minecraft address: `PI_IP_ADDRESS:25565`
- Administration panel: `http://PI_IP_ADDRESS:8080`
- System requirements and detailed Pi instructions: [Raspberry Pi installation guide](INSTALACION-PI.md)
- Full feature list and current limitations: [FEATURES.md](FEATURES.md)

## Install on Linux

The one-line installer supports Linux systems running **systemd** on `amd64`, `arm64`, and ARMv7, including Raspberry Pi OS on a Pi 2 Rev 1.1. It downloads the latest daemon release, verifies its SHA-256 checksum, installs and enables the service, and leaves existing world data untouched.

Create a **public GitHub repository** first, then replace `OWNER/REPOSITORY` below with its account and repository name:

The installer requires a published GitHub Release containing the daemon binaries. Follow [Publish and Sync with GitHub](#publish-and-sync-with-github) to create the repository and its first tagged release before running the install command.

```sh
curl -fsSL https://raw.githubusercontent.com/OWNER/REPOSITORY/main/scripts/install.sh \
	| sudo bash -s -- OWNER/REPOSITORY
```

The service starts immediately and launches automatically after reboot. Check it with:

```sh
sudo systemctl status autohost --no-pager
sudo journalctl -u autohost -f
```

To install the newest published daemon later, run:

```sh
sudo autohost-update
```

The updater verifies the download, keeps a copy of the previous binary for rollback, restarts the service, and preserves `/var/lib/autohost/data`.

## Install the Minecraft Mod

1. Install Java 21 and NeoForge for Minecraft 1.21.1.
2. Download `autohost-client-0.1.0.jar` from the project's GitHub Release.
3. Put the JAR in the `mods` folder of each player's Minecraft instance.
4. Add `PI_IP_ADDRESS:25565` to Multiplayer as a normal server and press **Join**.

The JAR is attached to each GitHub Release manually. The automated release workflow publishes only the Linux daemon and its systemd unit.

## Internet Access

For LAN play, no router port forwarding is required. For Internet play, forward TCP `25565` to the Pi and make the API reachable. The daemon listens on LAN port `8080`; the mod tries both API ports `8080` and `4000`, so either mapping works:

- Public TCP `8080` → Pi TCP `8080`
- Public TCP `4000` → Pi TCP `8080`

A reachable public IPv4 address is required for port forwarding. If the Pi's ISP uses CGNAT, use a VPN or a publicly reachable VPS instead. The administration API has no authentication; do not expose it to untrusted networks.

## How It Works

- The first player claims the host role and downloads the current world from the Pi, or creates a new `AutoHost` world if none exists.
- The host opens the world locally; the mod publishes it on LAN and maintains an outbound reverse tunnel to the Pi.
- Later players connect to the same Pi address. The Pi routes their Minecraft traffic through the tunnel to the host.
- The host saves and syncs changed world files every five minutes and when the session closes.
- The Pi retains up to four snapshots and caps their unique, deduplicated backup data at 4 GiB. The live world is stored separately.

## Publish and Sync with GitHub

The project folder is not linked to GitHub until you create a repository and set its remote. Git does not upload every file save automatically: commit and push changes to sync them. The release workflow runs automatically when you push a version tag.

On the Pi, install Git, create a dedicated SSH key, and add its **public** key to your GitHub account under **Settings → SSH and GPG keys**. Never share the private key:

```sh
sudo apt update && sudo apt install -y git
ssh-keygen -t ed25519 -C "autohost-pi"
cat ~/.ssh/id_ed25519.pub
ssh -T git@github.com
git config --global user.name "YOUR NAME"
git config --global user.email "YOUR_EMAIL"
```

Create an empty public repository on GitHub (do not initialize it with a README), then connect this existing project folder on the Pi:

```sh
cd /path/to/this/project
git init -b main
git add .
git commit -m "Initial AutoHost release"
git remote add origin git@github.com:sergiotejada4/minecraft-autohost.git
git push -u origin main
```

For later source changes, commit and push them. Git does not upload uncommitted file edits automatically; the `git push` is the synchronization step:

```sh
git add .
git commit -m "Describe the change"
git push
```

To publish daemon binaries, push a version tag. GitHub Actions tests the Go daemon, builds Linux binaries for `amd64`, `arm64`, and ARMv7, creates SHA-256 files, and publishes a GitHub Release:

```sh
git tag v0.1.0
git push origin v0.1.0
```

Open that release on GitHub and manually attach `autohost-client-0.1.0.jar`. The installer always downloads the latest non-draft release. Use a new version tag for each release.

## Build from Source

Requirements: Go 1.22 or later, JDK 21, and Gradle 8.8 or later.

```sh
go test ./...
go build -o autohost-daemon ./daemon

```

The client JAR is generated at `build/libs/autohost-client-0.1.0.jar`. The first Gradle build may take several minutes while Minecraft sources and mappings are prepared.

## Data and Backups

With the supplied systemd service, the live world is stored at `/var/lib/autohost/data/world`. Settings, the server icon, activity history, and deduplicated snapshot blobs are stored under `/var/lib/autohost/data`. Do not delete this directory when updating the daemon.

Snapshots are limited to four entries and 4 GiB of unique backup blobs. This limit does not include the live world. ZIP restores are available in the web panel when no host is active; the panel reports upload progress and the Pi validates the archive before replacing the world.
