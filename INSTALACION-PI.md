# AutoHost Installation on Raspberry Pi 2 Model B Rev 1.1

This guide installs the Go daemon on 32-bit Raspberry Pi OS Lite. The NeoForge 1.21.1 mod runs on the host PC, not on the Pi.

## Raspberry Pi and Networking

For play on the same local network, you do not need to open router ports. Choose the Pi's local IP address, for example `192.168.1.50`; the panel will be at `http://192.168.1.50:8080` and the Minecraft address will be `192.168.1.50:25565`.

To accept connections from the Internet, the Pi needs a reachable public IPv4 address. Forward TCP 25565 and the API port to its local IP. The API listens on port 8080; you can forward public port 8080 to LAN port 8080, or public port 4000 to LAN port 8080. The mod tries both API ports. If your ISP uses CGNAT, port forwarding will not work until you obtain a public IP; the host PC can still be behind CGNAT.

The Pi 2 Rev 1.1 has a Cortex-A7 ARMv7 CPU, 1 GB of RAM, and a 32-bit operating system. In Raspberry Pi Imager, install **Raspberry Pi OS Lite (32-bit)**, enable SSH, and connect it via Ethernet. Connect and verify:

```sh
ssh pi@IP_DE_LA_PI
getconf LONG_BIT
uname -m
sudo apt update
sudo apt install -y ca-certificates
```

`getconf LONG_BIT` should print `32`; `uname -m` usually returns `armv7l`.

## Build and Copy the Daemon

From PowerShell, in the project root, build the executable for ARMv7:

```powershell
$env:GOOS = 'linux'
$env:GOARCH = 'arm'
$env:GOARM = '7'
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags '-s -w' -o autohost-daemon-linux-armv7 ./daemon
Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM, Env:CGO_ENABLED
scp .\autohost-daemon-linux-armv7 ser@192.168.0.50:/tmp/autohost-daemon
scp .\deploy\autohost.service ser@192.168.0.50:/tmp/autohost.service
```

If a tool does not support a workspace on a UNC path, clone or copy the repository to a local Windows path before building.

On the Pi, create the service user and install the executable:

```sh
sudo adduser --system --group --home /var/lib/autohost --no-create-home autohost
sudo install -d -o autohost -g autohost -m 0750 /var/lib/autohost
sudo install -o root -g root -m 0755 /tmp/autohost-daemon /usr/local/bin/autohost-daemon
```

## Automatic Service

Copy `deploy/autohost.service` from the project to the Pi. By default, the daemon uses TCP 25565, HTTP 8080, and stores data in `/var/lib/autohost/data`:

```sh
sudo install -o root -g root -m 0644 /tmp/autohost.service /etc/systemd/system/autohost.service
sudo systemctl daemon-reload
sudo systemctl enable --now autohost
sudo systemctl status autohost --no-pager
sudo journalctl -u autohost -n 50 --no-pager
```

Check the local endpoint from the Pi:

```sh
curl -i http://127.0.0.1:8080/healthz
```

It should return `204 No Content`. Open the panel at `http://PI_IP_ADDRESS:8080`.

## Upgrade from an Earlier Version

Do not uninstall the service or delete `/var/lib/autohost/data`: it contains the world, settings, icon, and snapshots. If a session is active, close it from Minecraft and wait for the final upload to finish; confirm in the panel that there is no active host.

In PowerShell, from the project root, build the new daemon and copy it to the Pi under a temporary name:

```powershell
$env:GOOS = 'linux'
$env:GOARCH = 'arm'
$env:GOARM = '7'
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags '-s -w' -o autohost-daemon-linux-armv7 ./daemon
Remove-Item Env:GOOS, Env:GOARCH, Env:GOARM, Env:CGO_ENABLED
scp .\autohost-daemon-linux-armv7 ser@192.168.0.50:/tmp/autohost-daemon.new
```

Connect to the Pi over SSH and replace the executable. Keep a copy of the previous binary so you can roll back:

```sh
sudo systemctl stop autohost
sudo cp -a /usr/local/bin/autohost-daemon /usr/local/bin/autohost-daemon.bak
sudo install -o root -g root -m 0755 /tmp/autohost-daemon.new /usr/local/bin/autohost-daemon
sudo systemctl start autohost
sudo systemctl status autohost --no-pager
curl -i http://127.0.0.1:8080/healthz
sudo journalctl -u autohost -n 50 --no-pager
```

The endpoint should return `204 No Content`. If the daemon does not start, restore the previous executable:

```sh
sudo systemctl stop autohost
sudo install -o root -g root -m 0755 /usr/local/bin/autohost-daemon.bak /usr/local/bin/autohost-daemon
sudo systemctl start autohost
sudo systemctl status autohost --no-pager
```

If `deploy/autohost.service` also changed, copy the unit from PowerShell:

```powershell
scp .\deploy\autohost.service ser@192.168.0.50:/tmp/autohost.service
```

Then install it on the Pi and reload systemd:

```sh
sudo install -o root -g root -m 0644 /tmp/autohost.service /etc/systemd/system/autohost.service
sudo systemctl daemon-reload
sudo systemctl restart autohost
```

To update the mod, close Minecraft, run `gradle build` in the project, and replace `autohost-client-0.1.0.jar` in the `mods` folder of every player's instance with the new JAR. Do not delete `saves/AutoHost`; the host's local world syncs with the Pi when it connects. Existing settings are preserved; the new admin list starts empty and can be filled in from the panel.

## Router Configuration

For LAN-only play, do not change the router. For Internet access, reserve the Pi's local IP and forward **TCP 25565** and the public API port (**8080 → LAN 8080** or **4000 → LAN 8080**) to that IP. UDP is not required.

## Install and Configure the Mod

On every player's PC, install Java 21 and NeoForge for Minecraft 1.21.1. From the project root, run:

```powershell
gradle build
```

Copy `build/libs/autohost-client-0.1.0.jar` into the `mods` folder of each instance and launch Minecraft. In Multiplayer, add `PI_IP_ADDRESS:25565` as a normal server. You do not need to edit configuration files or open a LAN world manually.

The panel is available without login at `http://PI_IP_ADDRESS:8080`. From there, you can edit the MOTD and whitelist, manage world admins, upload a PNG/JPEG icon (resized to 64×64), view the host and players, download a ZIP, or restore a snapshot.

## First Session and Normal Use

1. Every player installs the JAR and adds `PI_IP_ADDRESS:25565` in Multiplayer.
2. The first player to press **Join** claims the host role. If the Pi already has `AutoHost`, the world is downloaded; otherwise, a new world is created automatically.
3. The mod opens the world, publishes it to LAN, and connects the tunnel. Players who are already waiting join the same address once the host is ready.
4. Later players simply press **Join** on the same server entry. Chat and the panel show who the host is and who joins or leaves.
5. The host runs `save-all flush` and uploads changed files and snapshots every five minutes. When closing the session, wait for the final upload to finish before the tunnel closes.

If the Pi already contains a world, it is authoritative: the first host downloads changed files into `saves/AutoHost`. Make a backup before restoring a snapshot from the panel; the next host receives the restored version when they press **Join**.

The Pi keeps at most **4 snapshots** and limits their backup blobs to **4 GiB**; when either limit is reached, the oldest snapshots are removed first. The quota counts unique data deduplicated by SHA-256 and does not include the current world, which is stored separately in `data/world`. Reserve additional space for that world. Snapshots are created when a session starts, every five minutes, and when a session closes, so substantial changes may cause the byte quota to be reached before four snapshots are retained. Check the SD card's free space.

## Troubleshooting

- Inactive status: the first player may still be downloading or creating the world. Check `sudo journalctl -u autohost -f`.
- Players cannot join over the Internet: verify that you have a public IPv4 address, check the firewall, and forward **TCP** port 25565.
- The panel does not open: make sure the Pi is powered on and use `http://PI_IP_ADDRESS:8080`.
- The host is not assigned: confirm all clients use the same Pi IPv4 address and that the API is reachable on public port 8080 or 4000.
- The Pi is behind CGNAT: request a public IPv4 address or use a VPS; the host PC's NAT does not affect its outbound tunnel.

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
