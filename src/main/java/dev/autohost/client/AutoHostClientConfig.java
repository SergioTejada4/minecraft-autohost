package dev.autohost.client;

import net.minecraft.client.Minecraft;

import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.concurrent.ConcurrentHashMap;

final class AutoHostClientConfig {
    static final String WORLD_FOLDER = "AutoHost";
    private static final int DEFAULT_API_PORT = 8080;
    private static final int ALTERNATE_API_PORT = 4000;
    private static final ConcurrentHashMap<String, Integer> PREFERRED_API_PORTS = new ConcurrentHashMap<>();
    private final String serverIp;

    private AutoHostClientConfig(String serverIp) {
        this.serverIp = serverIp;
    }

    static AutoHostClientConfig fromServerEntry(String entryAddress) throws IOException {
        String[] parts = entryAddress.trim().split(":", -1);
        if (parts.length < 1 || parts.length > 2) throw new IOException("AutoHost requires an IPv4 address.");
        if (parts.length == 2 && !parts[1].isEmpty() && !parts[1].equals("25565")) {
            throw new IOException("AutoHost uses port 25565.");
        }
        validateIp(parts[0]);
        return new AutoHostClientConfig(parts[0]);
    }

    String apiUrl() {
        return apiUrl(PREFERRED_API_PORTS.getOrDefault(serverIp, DEFAULT_API_PORT));
    }

    List<String> apiUrls() {
        int preferredPort = PREFERRED_API_PORTS.getOrDefault(serverIp, DEFAULT_API_PORT);
        int fallbackPort = preferredPort == DEFAULT_API_PORT ? ALTERNATE_API_PORT : DEFAULT_API_PORT;
        return List.of(apiUrl(preferredPort), apiUrl(fallbackPort));
    }

    void preferApiUrl(String apiUrl) {
        if (apiUrl.equals(apiUrl(DEFAULT_API_PORT))) {
            PREFERRED_API_PORTS.put(serverIp, DEFAULT_API_PORT);
        } else if (apiUrl.equals(apiUrl(ALTERNATE_API_PORT))) {
            PREFERRED_API_PORTS.put(serverIp, ALTERNATE_API_PORT);
        }
    }

    String serverIp() {
        return serverIp;
    }

    private String apiUrl(int port) {
        return "http://" + serverIp + ":" + port;
    }
    
    String tunnelAddress() {
        return serverIp + ":25565";
    }
    
    private static void validateIp(String ip) throws IOException {
        String[] octets = ip.split("\\.", -1);
        if (octets.length != 4) throw new IOException("Enter the Raspberry Pi's IPv4 address, for example 192.168.1.50.");
        for (String octet : octets) {
            try {
                int value = Integer.parseInt(octet);
                if (value < 0 || value > 255 || octet.isEmpty()) throw new NumberFormatException();
            } catch (NumberFormatException exception) {
                throw new IOException("Invalid IPv4 address: " + ip, exception);
            }
        }
    }
    
    Path worldPath() throws IOException {
        Path saves = Minecraft.getInstance().gameDirectory.toPath().resolve("saves").normalize();
        Path world = saves.resolve(WORLD_FOLDER).normalize();
        if (!world.startsWith(saves)) throw new IOException("World folder must be inside saves");
        return world;
    }
}