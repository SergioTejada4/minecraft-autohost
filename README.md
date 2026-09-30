# AutoHost P2P

Play a Minecraft 1.21.1 NeoForge world through a Raspberry Pi or linux system without keeping a dedicated server running. The mode is absolutely mod-compatible. The first player to join becomes the host; other players join through the same Multiplayer server entry. The project have been developed to work on Raspberry Pi but it should work in any other linux aswell.

The Raspberry Pi runs a small Go daemon for connection routing, world storage, and the web panel. Each player runs Minecraft with the NeoForge client mod. The Pi does not run Minecraft or Java.

- Minecraft address: `PI_IP_ADDRESS:25565`
- Administration panel: `http://PI_IP_ADDRESS:8080`

## Install on Linux

The one-line installer supports Linux systems running **systemd** on `amd64`, `arm64`, and ARMv7. It downloads the latest daemon release, verifies its SHA-256 checksum, installs and enables the service, and leaves existing world data untouched. It is highly recommended setting a static IP for the Linux system.

```sh
curl -fsSL https://raw.githubusercontent.com/sergiotejada4/minecraft-autohost/main/scripts/install.sh \
	| sudo bash -s -- sergiotejada4/minecraft-autohost
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
2. Download `autohost-client-x.x.x.jar` from the project's GitHub Release.
3. Put the JAR in the `mods` folder of each player's Minecraft instance. All the players MUST have the same modpack in order to be able to JOIN and to PRESERVE the worlds mod-related blocks. Deleting mods and joining as host will cause the dissapearance of mod-related blocks, be careful if this is not the intended action. Apart from that, adding or removing mods is totally fine.
4. Add `PI_IP_ADDRESS:25565` to Multiplayer as a normal server and press **Join**.

The JAR is attached to each GitHub Release manually, so each time you update the .jar, you must update the server side and viceversa. The automated release workflow publishes only the Linux daemon and its systemd unit.

## Internet Access

For LAN play, no router port forwarding is required. For Internet play, forward TCP `25565` to the Pi and make the API reachable. The daemon listens on LAN port `8080` and public `4000`.

- Public TCP `25565` → Pi TCP `25565`
- Public TCP `4000` → Pi TCP `8080`

A reachable public IPv4 address is required for port forwarding. If the Pi's ISP uses CGNAT, use a VPN or a publicly reachable VPS instead. The administration API has no authentication; do not expose it to untrusted networks.

## How It Works

- The first player claims the host role and downloads the current world from the Pi, or creates a new world if none exists.
- The host opens the world locally; the mod publishes it on LAN and maintains an outbound reverse tunnel to the Pi.
- Later players connect to the same Pi address. The Pi routes their Minecraft traffic through the tunnel to the host.
- The host saves and syncs changed world files every five minutes and when the session closes.
- The Pi retains up to four snapshots and caps their unique, deduplicated backup data at 4 GiB. The live world is stored separately.

## Server Customization

 Open the web panel at `http://PI_IP_ADDRESS:8080` from the local network. If accessing it remotely through the example router mapping, open `http://PUBLIC_IP:4000`. The Pi daemon listens on `8080` in both cases; the router translates public `4000` to Pi `8080`.

 ### Server

 - **Server message (MOTD):** set the text shown in the Minecraft Multiplayer list. The maximum length is 256 characters. The Pi keeps serving its configured MOTD while a host is active.
 - **Server icon:** upload a PNG or JPEG up to 8 MiB and 2048×2048 pixels. The Pi converts it to the 64×64 PNG required by Minecraft and uses it in server status responses.

 ### Player Access

 - **Whitelist:** enable it and enter Minecraft usernames, one per line. The host's integrated server enforces it when players join.
 - **World admins:** enter usernames, one per line. Admin names must be 1–16 characters using letters, digits, or underscores. They receive Minecraft operator level 4 in the hosted world.
 - Settings are stored on the Pi. The host loads them while preparing the world and refreshes them during the session.

 ### World & Backups

 - **Download ZIP:** downloads the current Pi world when no host is active.
 - **Restore ZIP:** uploads and validates an archive, then replaces the current Pi world. The panel shows upload percentage and reports while the Pi validates/restores it. The archive must contain a non-empty `level.dat` at its root; restoration is refused while a host is active.
 - **Snapshots:** select and restore an automatic restore point when no host is active. The Pi retains at most four snapshots and 4 GiB of unique deduplicated backup blobs. This quota does not include the live world.

 ### Activity

 The activity tab lists player joins and leaves, host tunnel connections/disconnections, and world backup/restore events. Press **Refresh** to load the latest entries.

 The panel and API currently have no authentication. Anyone who can reach the API may change these settings or world data, so expose the panel only on a trusted network or through a VPN.

## Data and Backups

With the supplied systemd service, the live world is stored at `/var/lib/autohost/data/world`. Settings, the server icon, activity history, and deduplicated snapshot blobs are stored under `/var/lib/autohost/data`. Do not delete this directory when updating the daemon.

Snapshots are limited to four entries and 4 GiB of unique backup blobs. This limit does not include the live world. ZIP restores are available in the web panel when no host is active; the panel reports upload progress and the Pi validates the archive before replacing the world.
