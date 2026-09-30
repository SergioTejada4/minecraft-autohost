package dev.autohost.client;

import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import net.minecraft.client.Minecraft;
import net.minecraft.client.gui.screens.GenericMessageScreen;
import net.minecraft.client.gui.screens.multiplayer.JoinMultiplayerScreen;
import net.minecraft.client.gui.screens.multiplayer.ServerSelectionList;
import net.minecraft.client.multiplayer.ServerData;
import net.minecraft.client.multiplayer.resolver.ServerAddress;
import net.minecraft.network.chat.Component;
import net.minecraft.world.Difficulty;
import net.minecraft.world.level.GameRules;
import net.minecraft.world.level.GameType;
import net.minecraft.world.level.LevelSettings;
import net.minecraft.world.level.WorldDataConfiguration;
import net.minecraft.world.level.levelgen.WorldOptions;
import net.minecraft.world.level.levelgen.presets.WorldPresets;

import java.io.IOException;
import java.net.ServerSocket;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

final class AutoHostCoordinator {
    private static final Logger LOGGER = Logger.getLogger(AutoHostCoordinator.class.getName());
    private static final String SERVICE_ID = "autohost-p2p";
    private static final HttpClient HTTP = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(4)).build();
    private static final ExecutorService COORDINATOR = Executors.newSingleThreadExecutor(task -> {
        Thread thread = new Thread(task, "autohost-coordinator");
        thread.setDaemon(true);
        return thread;
    });
    private static final AtomicBoolean JOIN_IN_PROGRESS = new AtomicBoolean();
    private static volatile AutoHostClientConfig hostConfig;
    private static volatile AutoHostClientConfig joinedConfig;
    private static volatile boolean hostedWorldWasOpen;
    private static volatile long nextStatusPoll;
    private static volatile long nextSnapshotAt;
    private static volatile long nextWhitelistAt;
    private static volatile long nextPublishAttemptAt;
    private static volatile String lastHostName = "";
    private static volatile boolean haveStatusBaseline;

    private AutoHostCoordinator() {
    }

    static boolean interceptJoin(JoinMultiplayerScreen screen, ServerData serverData) {
        AutoHostClientConfig config;
        try {
            config = AutoHostClientConfig.fromServerEntry(serverData.ip);
        } catch (IOException exception) {
            joinedConfig = null;
            haveStatusBaseline = false;
            return false;
        }
        if (!JOIN_IN_PROGRESS.compareAndSet(false, true)) return true;
        COORDINATOR.submit(() -> coordinateJoin(screen, serverData, config));
        return true;
    }

    private static void coordinateJoin(JoinMultiplayerScreen screen, ServerData serverData, AutoHostClientConfig config) {
        Minecraft minecraft = Minecraft.getInstance();
        try {
            Status status = readStatus(config);
            if (status == null) {
                joinDirect(screen, serverData, config);
                return;
            }
            if (status.hostActive()) {
                joinAutoHost(screen, serverData, config);
                return;
            }
            long deadline = System.nanoTime() + Duration.ofMinutes(15).toNanos();
            while (System.nanoTime() < deadline) {
                status = readStatus(config);
                if (status == null) {
                    joinDirect(screen, serverData, config);
                    return;
                }
                if (status.hostActive()) {
                    joinAutoHost(screen, serverData, config);
                    return;
                }
                if (!status.syncing() && claimHost(config)) {
                    prepareNewHost(minecraft, screen, config);
                    return;
                }
                Thread.sleep(1000);
            }
            minecraft.execute(() -> {
                minecraft.setScreen(screen);
                showChat("AutoHost: host assignment timed out. Try joining again.");
            });
        } catch (Exception exception) {
            LOGGER.log(Level.WARNING, "AutoHost could not coordinate server join", exception);
            joinDirect(screen, serverData, config);
        } finally {
            JOIN_IN_PROGRESS.set(false);
        }
    }

    private static Status readStatus(AutoHostClientConfig config) throws Exception {
        for (String apiUrl : config.apiUrls()) {
            HttpRequest request = HttpRequest.newBuilder(URI.create(apiUrl + "/api/status"))
                    .timeout(Duration.ofSeconds(5))
                    .GET()
                    .build();
            HttpResponse<String> response;
            try {
                response = HTTP.send(request, HttpResponse.BodyHandlers.ofString());
            } catch (IOException exception) {
                continue;
            }
            if (response.statusCode() != 200) continue;
            JsonObject json = JsonParser.parseString(response.body()).getAsJsonObject();
            if (!json.has("service") || !SERVICE_ID.equals(json.get("service").getAsString())) continue;
            config.preferApiUrl(apiUrl);
            return new Status(json.has("hostActive") && json.get("hostActive").getAsBoolean(),
                    json.has("syncing") && json.get("syncing").getAsBoolean());
        }
        return null;
    }

    private static boolean claimHost(AutoHostClientConfig config) throws Exception {
        HttpRequest request = HttpRequest.newBuilder(URI.create(config.apiUrl() + "/api/host/claim"))
                .timeout(Duration.ofSeconds(5))
                .POST(HttpRequest.BodyPublishers.noBody())
                .build();
        return HTTP.send(request, HttpResponse.BodyHandlers.discarding()).statusCode() == 201;
    }

    private static void joinDirect(JoinMultiplayerScreen screen, ServerData serverData,
                                   AutoHostClientConfig config) {
        Minecraft minecraft = Minecraft.getInstance();
        minecraft.execute(() -> {
            joinedConfig = null;
            haveStatusBaseline = false;
            net.minecraft.client.gui.screens.ConnectScreen.startConnecting(screen, minecraft,
                    ServerAddress.parseString(serverData.ip), serverData, false, null);
        });
    }

    private static void joinAutoHost(JoinMultiplayerScreen screen, ServerData serverData,
                                     AutoHostClientConfig config) {
        Minecraft minecraft = Minecraft.getInstance();
        minecraft.execute(() -> {
            joinedConfig = config;
            haveStatusBaseline = false;
            try {
                PlayerConnectionProxy.connect(screen, minecraft, serverData, config);
            } catch (IOException exception) {
                LOGGER.log(Level.WARNING, "Could not start the AutoHost player proxy", exception);
                minecraft.setScreen(screen);
                showChat("AutoHost: could not open the local player tunnel.");
            }
        });
    }

    private static void prepareNewHost(Minecraft minecraft, JoinMultiplayerScreen screen,
                                       AutoHostClientConfig config) {
        hostConfig = config;
        minecraft.execute(() -> minecraft.setScreen(new GenericMessageScreen(
                Component.literal("AutoHost: syncing the world and preparing the session..."))));
        WorldSyncClient.prepareHostAsync(config, (worldExists, error) -> minecraft.execute(() -> {
            if (error != null) {
                LOGGER.warning(error);
                minecraft.setScreen(screen);
                showChat("AutoHost: could not prepare the world: " + error);
                hostConfig = null;
                releaseHostClaim(config);
                return;
            }
            if (worldExists) {
                minecraft.createWorldOpenFlows().openWorld(AutoHostClientConfig.WORLD_FOLDER,
                        () -> minecraft.setScreen(screen));
            } else {
                LevelSettings settings = new LevelSettings(AutoHostClientConfig.WORLD_FOLDER,
                        GameType.SURVIVAL, false, Difficulty.NORMAL, false,
                        new GameRules(), WorldDataConfiguration.DEFAULT);
                minecraft.createWorldOpenFlows().createFreshLevel(AutoHostClientConfig.WORLD_FOLDER,
                        settings, WorldOptions.defaultWithRandomSeed(),
                        WorldPresets::createNormalWorldDimensions, screen);
            }
        }));
        COORDINATOR.submit(() -> maintainHostClaim(config));
    }

    private static void maintainHostClaim(AutoHostClientConfig config) {
        long deadline = System.nanoTime() + Duration.ofMinutes(15).toNanos();
        while (config.equals(hostConfig) && System.nanoTime() < deadline) {
            try {
                Thread.sleep(20_000);
                if (!config.equals(hostConfig)) return;
                Status status = readStatus(config);
                if (status != null && status.hostActive()) return;
                if (status != null) claimHost(config);
            } catch (InterruptedException exception) {
                Thread.currentThread().interrupt();
                return;
            } catch (Exception exception) {
                LOGGER.log(Level.FINE, "Could not renew AutoHost host claim", exception);
            }
        }
        if (config.equals(hostConfig)) {
            hostConfig = null;
            releaseHostClaim(config);
            showChat("AutoHost: host preparation expired.");
        }
    }

    private static void releaseHostClaim(AutoHostClientConfig config) {
        COORDINATOR.submit(() -> {
            try {
                HttpRequest request = HttpRequest.newBuilder(URI.create(config.apiUrl() + "/api/host/release"))
                        .timeout(Duration.ofSeconds(5))
                        .POST(HttpRequest.BodyPublishers.noBody())
                        .build();
                HTTP.send(request, HttpResponse.BodyHandlers.discarding());
            } catch (Exception exception) {
                LOGGER.log(Level.FINE, "Could not release AutoHost claim", exception);
            }
        });
    }

    static void onClientTick(Minecraft minecraft) {
        var integratedServer = minecraft.getSingleplayerServer();
        AutoHostClientConfig currentHost = hostConfig;
        if (integratedServer != null && currentHost != null) {
            hostedWorldWasOpen = true;
            if (minecraft.player == null) return;
            if (!integratedServer.isPublished() && System.nanoTime() >= nextPublishAttemptAt) {
                nextPublishAttemptAt = System.nanoTime() + Duration.ofSeconds(5).toNanos();
                int port = findAvailablePort();
                if (port == 0 || !integratedServer.publishServer(GameType.SURVIVAL, true, port)) {
                    showChat("AutoHost: could not open the world on LAN.");
                    return;
                }
                ClientCommands.startTunnel(currentHost.tunnelAddress(), port);
                integratedServer.execute(() -> {
                    integratedServer.getCommands().performPrefixedCommand(
                        integratedServer.createCommandSourceStack(), "save-all flush");
                    minecraft.execute(() -> WorldSyncClient.pushSnapshotAsync(currentHost));
                });
                nextSnapshotAt = System.nanoTime() + Duration.ofMinutes(5).toNanos();
                nextWhitelistAt = System.nanoTime() + Duration.ofSeconds(30).toNanos();
                showChat("AutoHost: you are the host. Other players can join using the Raspberry Pi's IP.");
            }
            if (System.nanoTime() >= nextWhitelistAt) {
                nextWhitelistAt = System.nanoTime() + Duration.ofSeconds(30).toNanos();
                WorldSyncClient.refreshWhitelistAsync(currentHost);
            }
            if (System.nanoTime() >= nextSnapshotAt && !WorldSyncClient.isRunning()) {
                nextSnapshotAt = System.nanoTime() + Duration.ofMinutes(5).toNanos();
                integratedServer.execute(() -> {
                    integratedServer.getCommands().performPrefixedCommand(
                        integratedServer.createCommandSourceStack(), "save-all flush");
                    minecraft.execute(() -> WorldSyncClient.pushSnapshotAsync(currentHost));
                });
            }
            return;
        }
        if (hostedWorldWasOpen && integratedServer == null) {
            hostedWorldWasOpen = false;
            AutoHostClientConfig previousHost = hostConfig;
            hostConfig = null;
            if (previousHost != null) {
                WorldSyncClient.pushSnapshotAsync(previousHost, ClientCommands::stopTunnel);
            } else {
                ClientCommands.stopTunnel();
            }
        }
        if (joinedConfig != null && minecraft.getConnection() != null
            && minecraft.getConnection().getConnection().isConnected()
                && System.nanoTime() >= nextStatusPoll) {
            nextStatusPoll = System.nanoTime() + Duration.ofSeconds(2).toNanos();
            pollPlayerStatus(joinedConfig);
        } else if (joinedConfig != null && minecraft.getConnection() == null && hostedWorldWasOpen == false
                && minecraft.screen instanceof net.minecraft.client.gui.screens.TitleScreen) {
            joinedConfig = null;
            haveStatusBaseline = false;
        }
    }

    private static int findAvailablePort() {
        try (ServerSocket socket = new ServerSocket(0)) {
            return socket.getLocalPort();
        } catch (IOException exception) {
            LOGGER.log(Level.WARNING, "Could not allocate a local LAN port", exception);
            return 0;
        }
    }

    private static void pollPlayerStatus(AutoHostClientConfig config) {
        COORDINATOR.submit(() -> {
            try {
                StatusSnapshot snapshot = readPlayerStatus(config);
                if (snapshot == null) return;
                if (!haveStatusBaseline) {
                    lastHostName = snapshot.hostName();
                    haveStatusBaseline = true;
                    if (!lastHostName.isBlank()) {
                        showChat("AutoHost: current host is " + lastHostName + ".");
                    }
                    return;
                }
                String host = snapshot.hostName().isBlank() ? "unknown" : snapshot.hostName();
                if (!snapshot.hostName().equals(lastHostName)) {
                    if (snapshot.hostName().isBlank()) showChat("AutoHost: the host disconnected.");
                    else showChat("AutoHost: the host is now " + host + ".");
                    lastHostName = snapshot.hostName();
                }
            } catch (Exception exception) {
                LOGGER.log(Level.FINE, "Could not poll AutoHost status", exception);
            }
        });
    }

    private static StatusSnapshot readPlayerStatus(AutoHostClientConfig config) throws Exception {
        for (String apiUrl : config.apiUrls()) {
            HttpRequest request = HttpRequest.newBuilder(URI.create(apiUrl + "/api/status"))
                    .timeout(Duration.ofSeconds(5)).GET().build();
            HttpResponse<String> response;
            try {
                response = HTTP.send(request, HttpResponse.BodyHandlers.ofString());
            } catch (IOException exception) {
                continue;
            }
            if (response.statusCode() != 200) continue;
            JsonObject json = JsonParser.parseString(response.body()).getAsJsonObject();
            if (!SERVICE_ID.equals(json.get("service").getAsString())) continue;
            config.preferApiUrl(apiUrl);
            String host = json.has("hostName") ? json.get("hostName").getAsString() : "";
            return new StatusSnapshot(host);
        }
        return null;
    }

    private static void showChat(String text) {
        Minecraft minecraft = Minecraft.getInstance();
        minecraft.execute(() -> minecraft.gui.getChat().addMessage(Component.literal(text)));
    }

    private record Status(boolean hostActive, boolean syncing) {
    }

    private record StatusSnapshot(String hostName) {
    }
}