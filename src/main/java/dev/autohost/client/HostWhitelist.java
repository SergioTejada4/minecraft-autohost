package dev.autohost.client;

import com.google.gson.JsonArray;
import com.google.gson.JsonObject;
import dev.autohost.AutoHostMod;
import net.minecraft.client.Minecraft;
import net.minecraft.server.MinecraftServer;
import net.minecraft.network.chat.Component;
import net.minecraft.server.level.ServerPlayer;
import net.minecraft.server.players.ServerOpListEntry;
import net.neoforged.api.distmarker.Dist;
import net.neoforged.bus.api.SubscribeEvent;
import net.neoforged.fml.common.EventBusSubscriber;
import net.neoforged.neoforge.event.entity.player.PlayerEvent;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicReference;
import java.util.logging.Level;
import java.util.logging.Logger;

@EventBusSubscriber(modid = AutoHostMod.MOD_ID, value = Dist.CLIENT)
public final class HostWhitelist {
    private static final AtomicReference<Access> CURRENT = new AtomicReference<>(new Access(false, Set.of(), Set.of()));
    private static final Map<String, ServerPlayer> CONNECTED = new ConcurrentHashMap<>();
    private static final HttpClient HTTP = HttpClient.newHttpClient();
    private static final ExecutorService REPORTER = Executors.newSingleThreadExecutor(task -> {
        Thread thread = new Thread(task, "autohost-player-report");
        thread.setDaemon(true);
        return thread;
    });
    private static final Logger LOGGER = Logger.getLogger(HostWhitelist.class.getName());
    private static volatile ReportConfig reportConfig;

    private HostWhitelist() {
    }

    static void configure(boolean enabled, Set<String> allowedPlayers, Set<String> admins, AutoHostClientConfig config) {
        Access access = new Access(enabled, Set.copyOf(allowedPlayers), Set.copyOf(admins));
        CURRENT.set(access);
        reportConfig = new ReportConfig(config, Minecraft.getInstance().getUser().getName());
        for (ServerPlayer player : CONNECTED.values()) {
            if (!Minecraft.getInstance().getUser().getName().equalsIgnoreCase(player.getGameProfile().getName())) {
                applyOperatorStatus(player, access.admins().contains(player.getGameProfile().getName().toLowerCase(Locale.ROOT)));
            }
        }
        if (!CONNECTED.isEmpty()) reportPlayers();
    }

    @SubscribeEvent
    public static void onPlayerLogin(PlayerEvent.PlayerLoggedInEvent event) {
        if (!(event.getEntity() instanceof ServerPlayer player)) return;
        var server = player.getServer();
        if (server == null || !server.isSingleplayer()) return;
        Access access = CURRENT.get();
        String playerName = player.getGameProfile().getName().toLowerCase(Locale.ROOT);
        String owner = Minecraft.getInstance().getUser().getName();
        boolean isOwner = owner.equalsIgnoreCase(playerName);
        if (access.whitelistEnabled() && !isOwner && !access.allowedPlayers().contains(playerName)) {
            player.connection.disconnect(Component.literal("You are not on this world's whitelist."));
            return;
        }
        CONNECTED.put(player.getGameProfile().getName(), player);
        if (!isOwner) applyOperatorStatus(player, access.admins().contains(playerName));
        reportPlayers();
        server.getPlayerList().broadcastSystemMessage(Component.literal(
            "AutoHost: " + player.getGameProfile().getName() + " joined. Current host: " + owner + "."), false);
    }

    @SubscribeEvent
    public static void onPlayerLogout(PlayerEvent.PlayerLoggedOutEvent event) {
        if (!(event.getEntity() instanceof ServerPlayer player)) return;
        MinecraftServer server = player.getServer();
        if (server == null || !server.isSingleplayer()) return;
        CONNECTED.remove(player.getGameProfile().getName());
        reportPlayers();
        String owner = Minecraft.getInstance().getUser().getName();
        server.getPlayerList().broadcastSystemMessage(Component.literal(
            "AutoHost: " + player.getGameProfile().getName() + " left. Current host: " + owner + "."), false);
    }

    private static void reportPlayers() {
        ReportConfig report = reportConfig;
        if (report == null) return;
        JsonObject body = new JsonObject();
        JsonArray players = new JsonArray();
        new TreeSet<>(CONNECTED.keySet()).forEach(players::add);
        body.add("players", players);
        body.addProperty("hostName", report.hostName());
        REPORTER.submit(() -> {
            AutoHostClientConfig config = report.config();
            for (String apiUrl : config.apiUrls()) {
                HttpRequest request = HttpRequest.newBuilder(URI.create(apiUrl + "/api/host/players"))
                        .header("Content-Type", "application/json")
                        .PUT(HttpRequest.BodyPublishers.ofString(body.toString(), StandardCharsets.UTF_8))
                        .build();
                try {
                    HttpResponse<Void> response = HTTP.send(request, HttpResponse.BodyHandlers.discarding());
                    config.preferApiUrl(apiUrl);
                    if (response.statusCode() < 200 || response.statusCode() >= 300) {
                        LOGGER.warning("Could not update AutoHost player list: HTTP " + response.statusCode());
                    }
                    return;
                } catch (Exception exception) {
                    LOGGER.log(Level.FINE, "Could not report players through " + apiUrl, exception);
                }
            }
            LOGGER.warning("Could not update AutoHost player list on ports 8080 or 4000.");
        });
    }

    private static void applyOperatorStatus(ServerPlayer player, boolean operator) {
        MinecraftServer server = player.getServer();
        if (server == null) return;
        server.execute(() -> {
            var operators = server.getPlayerList().getOps();
            if (operator) operators.add(new ServerOpListEntry(player.getGameProfile(), 4, false));
            else operators.remove(player.getGameProfile());
        });
    }

    private record Access(boolean whitelistEnabled, Set<String> allowedPlayers, Set<String> admins) {
    }

    private record ReportConfig(AutoHostClientConfig config, String hostName) {
    }
}