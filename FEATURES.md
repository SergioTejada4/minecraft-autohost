# AutoHost P2P Features

AutoHost turns the first player who joins a Raspberry Pi address into the temporary Minecraft host. The Pi runs a lightweight Go daemon; Minecraft and the NeoForge client mod run on player PCs.

## Join Flow

- Every player installs the NeoForge 1.21.1 client JAR and adds the Pi address as a normal Multiplayer server: `PI_IP_ADDRESS:25565`.
- The first player claims the host role. The client downloads the Pi's current world, or creates `AutoHost` if no world exists, then opens it locally and starts the reverse tunnel.
- Later players use the same server entry. Their client proxies the Minecraft connection through the Pi to the active host's integrated server.
- When the host closes the world, the final upload finishes and the tunnel closes. Another player can become host on the next join.

## Raspberry Pi Daemon

- TCP `25565` handles Minecraft status pings, login routing, and the reverse tunnel.
- The daemon answers status requests with the configured MOTD and server icon, including while a host is active.
- HTTP `8080` provides the coordination API and the administration panel. The client also tries API port `4000`, so a router can forward public `4000` to LAN `8080`.
- The panel reports host state, connections, player names, world size, backup usage, and activity history.
- The panel manages the MOTD, icon, whitelist, world operators, ZIP backups, and snapshot restore.

## World Sync and Backups

- Clients compare SHA-256 manifests and transfer only files that are new or changed.
- The host runs `save-all flush` before scheduled syncs, then uploads changed world files and creates a snapshot every five minutes and when the session closes.
- The current world is stored at `/var/lib/autohost/data/world` by the default systemd service.
- The daemon retains at most four snapshots and limits unique, deduplicated backup blobs to 4 GiB. The live world uses additional storage.
- ZIP restore validates the archive and requires a non-empty root `level.dat` before replacing the current world. The web panel reports upload progress and restore status.

## Access Controls

- The optional whitelist is enforced by the host's integrated server.
- Admin usernames configured in the panel receive Minecraft operator level 4 in the hosted world.
- The panel and API do not provide authentication. Do not expose port `8080` or its forwarded public equivalent to untrusted networks; use a VPN or add authentication before public deployment.

## Installation and Updates

- The Linux installer supports systemd on `amd64`, `arm64`, and ARMv7 Linux systems.
- Installation downloads the latest GitHub Release binary, verifies its SHA-256 checksum, installs/enables the systemd service, and preserves existing world data.
- `sudo autohost-update` downloads and installs the latest daemon release.
- GitHub Actions publishes daemon binaries when a `v*` tag is pushed. The Minecraft client JAR is not published by that workflow; attach it manually to the GitHub Release.
