package dev.autohost.client;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;

import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.BiConsumer;

final class WorldSyncClient {
    private static final long MAX_WORLD_FILE_BYTES = 8L * 1024 * 1024 * 1024;
    private static final HttpClient HTTP = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(15))
            .followRedirects(HttpClient.Redirect.NEVER)
            .build();
    private static final AtomicBoolean RUNNING = new AtomicBoolean();
    private static final AtomicBoolean SETTINGS_REFRESH_RUNNING = new AtomicBoolean();
    private static final ExecutorService EXECUTOR = Executors.newSingleThreadExecutor(task -> {
        Thread thread = new Thread(task, "autohost-world-sync");
        thread.setDaemon(true);
        return thread;
    });
    private static final ExecutorService SETTINGS_EXECUTOR = Executors.newSingleThreadExecutor(task -> {
        Thread thread = new Thread(task, "autohost-settings-sync");
        thread.setDaemon(true);
        return thread;
    });

    private WorldSyncClient() {
    }

    static boolean isRunning() {
        return RUNNING.get();
    }

    static void refreshWhitelistAsync(AutoHostClientConfig config) {
        if (!SETTINGS_REFRESH_RUNNING.compareAndSet(false, true)) return;
        SETTINGS_EXECUTOR.submit(() -> {
            try {
                loadWhitelist(config);
            } catch (Exception exception) {
                System.getLogger(WorldSyncClient.class.getName()).log(System.Logger.Level.WARNING,
                        "Could not refresh AutoHost whitelist", exception);
            } finally {
                SETTINGS_REFRESH_RUNNING.set(false);
            }
        });
    }

    static void prepareHostAsync(AutoHostClientConfig config, BiConsumer<Boolean, String> completion) {
        if (!RUNNING.compareAndSet(false, true)) {
            completion.accept(false, "A synchronization is already in progress.");
            return;
        }
        EXECUTOR.submit(() -> {
            try {
                boolean worldExists = pullOrSeed(config);
                if (worldExists) createSnapshot(config);
                loadWhitelist(config);
                completion.accept(worldExists, null);
            } catch (Exception exception) {
                completion.accept(false, exception.getMessage() == null ? "World synchronization failed." : exception.getMessage());
            } finally {
                RUNNING.set(false);
            }
        });
    }

    static void pushSnapshotAsync(AutoHostClientConfig config) {
        pushSnapshotAsync(config, null);
    }

    static void pushSnapshotAsync(AutoHostClientConfig config, Runnable completion) {
        EXECUTOR.submit(() -> {
            if (!RUNNING.compareAndSet(false, true)) {
                pushSnapshotAsync(config, completion);
                return;
            }
            try {
                push(config);
            } catch (Exception exception) {
                System.getLogger(WorldSyncClient.class.getName()).log(System.Logger.Level.ERROR,
                        "Could not upload world to AutoHost", exception);
            } finally {
                RUNNING.set(false);
                if (completion != null) completion.run();
            }
        });
    }

    private static void loadWhitelist(AutoHostClientConfig config) throws Exception {
        HttpResponse<String> response = send(config, "/api/admin/settings", "GET",
                HttpRequest.BodyPublishers.noBody(), HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
        requireSuccess(response.statusCode(), "load whitelist settings");
        JsonObject settings = JsonParser.parseString(response.body()).getAsJsonObject();
        Set<String> allowed = new java.util.HashSet<>();
        if (settings.has("allowedPlayers") && settings.get("allowedPlayers").isJsonArray()) {
            for (JsonElement player : settings.getAsJsonArray("allowedPlayers")) {
                allowed.add(player.getAsString().toLowerCase(java.util.Locale.ROOT));
            }
        }
        Set<String> admins = new java.util.HashSet<>();
        if (settings.has("adminPlayers") && settings.get("adminPlayers").isJsonArray()) {
            for (JsonElement player : settings.getAsJsonArray("adminPlayers")) {
                admins.add(player.getAsString().toLowerCase(java.util.Locale.ROOT));
            }
        }
        HostWhitelist.configure(settings.has("whitelistEnabled") && settings.get("whitelistEnabled").getAsBoolean(),
            allowed, admins, config);
    }

    private static boolean pullOrSeed(AutoHostClientConfig config) throws Exception {
        requireConfiguration(config);
        Path world = config.worldPath();
        Files.createDirectories(world);
        Map<String, RemoteFile> remote = getRemoteManifest(config);
        if (remote.isEmpty()) {
            Map<String, String> local = scanWorld(world);
            if (!local.containsKey("level.dat")) {
                return false;
            }
            if (!local.isEmpty()) {
                uploadChanged(config, local, remote);
            }
            return true;
        }
        if (!remote.containsKey("level.dat")) {
            throw new IOException("The Pi's world does not contain level.dat.");
        }
        Set<String> remotePaths = remote.keySet();
        for (Map.Entry<String, RemoteFile> entry : remote.entrySet()) {
            Path target = resolveWorldPath(world, entry.getKey());
            if (!Files.isRegularFile(target) || !sha256(target).equals(entry.getValue().hash())) {
                downloadFile(config, entry.getKey(), entry.getValue().hash(), target);
            }
        }
        for (String localPath : scanWorld(world).keySet()) {
            if (!remotePaths.contains(localPath)) {
                Files.deleteIfExists(resolveWorldPath(world, localPath));
            }
        }
        deleteEmptyDirectories(world);
        if (!Files.isRegularFile(world.resolve("level.dat"))) {
            throw new IOException("The Pi's world does not contain level.dat.");
        }
        return true;
    }

    private static void push(AutoHostClientConfig config) throws Exception {
        requireConfiguration(config);
        Path world = config.worldPath();
        if (!Files.isDirectory(world)) {
            throw new IOException("The local saves/AutoHost world does not exist.");
        }
        Map<String, String> local = scanWorld(world);
        Map<String, RemoteFile> remote = getRemoteManifest(config);
        uploadChanged(config, local, remote);
        for (String remotePath : remote.keySet()) {
            if (!local.containsKey(remotePath)) {
                HttpResponse<Void> response = send(config, "/api/world/files/" + encodePath(remotePath),
                        "DELETE", HttpRequest.BodyPublishers.noBody(), HttpResponse.BodyHandlers.discarding());
                requireSuccess(response.statusCode(), "delete " + remotePath);
            }
        }
        createSnapshot(config);
    }

    private static void createSnapshot(AutoHostClientConfig config) throws Exception {
        HttpResponse<Void> response = send(config, "/api/admin/snapshots", "POST",
                HttpRequest.BodyPublishers.noBody(), HttpResponse.BodyHandlers.discarding());
        requireSuccess(response.statusCode(), "create a world snapshot");
    }

    private static void uploadChanged(AutoHostClientConfig config, Map<String, String> local,
                                      Map<String, RemoteFile> remote) throws Exception {
        Path root = config.worldPath();
        for (Map.Entry<String, String> entry : local.entrySet()) {
            RemoteFile previous = remote.get(entry.getKey());
            if (previous != null && previous.hash().equals(entry.getValue())) {
                continue;
            }
            Path source = resolveWorldPath(root, entry.getKey());
            if (Files.size(source) > MAX_WORLD_FILE_BYTES) {
                throw new IOException("World file exceeds 8 GiB: " + entry.getKey());
            }
            HttpResponse<Void> response = send(config, "/api/world/files/" + encodePath(entry.getKey()),
                    "PUT", HttpRequest.BodyPublishers.ofFile(source), HttpResponse.BodyHandlers.discarding());
            requireSuccess(response.statusCode(), "upload " + entry.getKey());
        }
    }

    private static Map<String, RemoteFile> getRemoteManifest(AutoHostClientConfig config) throws Exception {
        HttpResponse<String> response = send(config, "/api/world/manifest", "GET",
                HttpRequest.BodyPublishers.noBody(), HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));
        requireSuccess(response.statusCode(), "load world manifest");
        JsonObject body = JsonParser.parseString(response.body()).getAsJsonObject();
        Map<String, RemoteFile> files = new HashMap<>();
        for (JsonElement element : body.getAsJsonArray("files")) {
            JsonObject file = element.getAsJsonObject();
            String path = file.get("path").getAsString();
            validateRelativePath(path);
            files.put(path, new RemoteFile(file.get("sha256").getAsString(), file.get("size").getAsLong()));
        }
        return files;
    }

    private static void downloadFile(AutoHostClientConfig config, String relative, String expectedHash, Path target) throws Exception {
        Files.createDirectories(target.getParent());
        Path temporary = Files.createTempFile(target.getParent(), ".autohost-download-", ".tmp");
        try {
            HttpResponse<Path> response = send(config, "/api/world/files/" + encodePath(relative), "GET",
                    HttpRequest.BodyPublishers.noBody(), HttpResponse.BodyHandlers.ofFile(temporary));
            requireSuccess(response.statusCode(), "download " + relative);
            if (!sha256(temporary).equals(expectedHash)) {
                throw new IOException("El hash SHA-256 no coincide al descargar " + relative);
            }
            try {
                Files.move(temporary, target, StandardCopyOption.REPLACE_EXISTING, StandardCopyOption.ATOMIC_MOVE);
            } catch (AtomicMoveNotSupportedException exception) {
                Files.move(temporary, target, StandardCopyOption.REPLACE_EXISTING);
            }
        } finally {
            Files.deleteIfExists(temporary);
        }
    }

    private static Map<String, String> scanWorld(Path root) throws IOException, NoSuchAlgorithmException {
        Map<String, String> files = new HashMap<>();
        try (var paths = Files.walk(root)) {
            for (Path path : paths.toList()) {
                if (!Files.isRegularFile(path) || Files.isSymbolicLink(path) || path.getFileName().toString().equals("session.lock")) {
                    continue;
                }
                String relative = root.relativize(path).toString().replace('\\', '/');
                validateRelativePath(relative);
                files.put(relative, sha256(path));
            }
        }
        return files;
    }

    private static String sha256(Path path) throws IOException, NoSuchAlgorithmException {
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        try (InputStream input = Files.newInputStream(path)) {
            byte[] buffer = new byte[64 * 1024];
            int read;
            while ((read = input.read(buffer)) >= 0) {
                if (read > 0) digest.update(buffer, 0, read);
            }
        }
        return HexFormat.of().formatHex(digest.digest());
    }

    private static void deleteEmptyDirectories(Path root) throws IOException {
        try (var paths = Files.walk(root)) {
            for (Path path : paths.sorted((left, right) -> right.getNameCount() - left.getNameCount()).toList()) {
                if (!path.equals(root) && Files.isDirectory(path)) {
                    try (var children = Files.list(path)) {
                        if (children.findAny().isEmpty()) Files.deleteIfExists(path);
                    }
                }
            }
        }
    }

    private static Path resolveWorldPath(Path root, String relative) throws IOException {
        validateRelativePath(relative);
        Path normalizedRoot = root.toAbsolutePath().normalize();
        Path resolved = normalizedRoot.resolve(relative.replace('/', java.io.File.separatorChar)).normalize();
        if (!resolved.startsWith(normalizedRoot)) throw new IOException("Path is outside the world: " + relative);
        return resolved;
    }

    private static void validateRelativePath(String relative) throws IOException {
        Path path = Path.of(relative.replace('/', java.io.File.separatorChar)).normalize();
        if (relative.isBlank() || path.isAbsolute() || path.startsWith("..") || relative.contains("\\") || relative.contains("\u0000")) {
            throw new IOException("Invalid world path: " + relative);
        }
    }

    private static String encodePath(String path) {
        return java.util.Arrays.stream(path.split("/", -1))
                .map(segment -> URLEncoder.encode(segment, StandardCharsets.UTF_8).replace("+", "%20"))
                .collect(java.util.stream.Collectors.joining("/"));
    }

    private static <T> HttpResponse<T> send(AutoHostClientConfig config, String path, String method,
                                             HttpRequest.BodyPublisher body, HttpResponse.BodyHandler<T> handler) throws Exception {
        IOException lastError = null;
        for (String apiUrl : config.apiUrls()) {
            URI uri = URI.create(apiUrl + path);
            HttpRequest.Builder builder = HttpRequest.newBuilder(uri)
                .timeout(Duration.ofMinutes(10));
            if (method.equals("GET")) builder.GET();
            else builder.method(method, body);
            try {
                HttpResponse<T> response = HTTP.send(builder.build(), handler);
                config.preferApiUrl(apiUrl);
                return response;
            } catch (IOException exception) {
                lastError = exception;
            }
        }
        if (lastError != null) throw lastError;
        throw new IOException("Could not connect to the AutoHost API.");
    }

    private static void requireSuccess(int status, String operation) throws IOException {
        if (status < 200 || status >= 300) throw new IOException("AutoHost no pudo " + operation + " (HTTP " + status + ")");
    }

    private static void requireConfiguration(AutoHostClientConfig config) throws IOException {
        AutoHostClientConfig.fromServerEntry(config.serverIp());
    }

    private record RemoteFile(String hash, long size) {
    }
}