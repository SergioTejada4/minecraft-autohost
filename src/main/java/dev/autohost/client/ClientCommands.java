package dev.autohost.client;

import net.minecraft.network.chat.Component;

import java.util.concurrent.atomic.AtomicReference;

public final class ClientCommands {
    private static final AtomicReference<TunnelClient> ACTIVE_TUNNEL = new AtomicReference<>();

    private ClientCommands() {
    }

    static boolean startTunnel(String address, int lanPort) {
        TunnelClient client = new TunnelClient(address, lanPort);
        if (!ACTIVE_TUNNEL.compareAndSet(null, client)) {
            return false;
        }
        Thread thread = new Thread(() -> {
            try {
                client.run();
            } finally {
                ACTIVE_TUNNEL.compareAndSet(client, null);
            }
        }, "autohost-tunnel");
        thread.setDaemon(true);
        thread.start();
        return true;
    }

    private static int stopTunnel(net.minecraft.commands.CommandSourceStack source) {
        if (!stopTunnel()) {
            source.sendFailure(Component.literal("No AutoHost tunnel is active."));
            return 0;
        }
        source.sendSuccess(() -> Component.literal("AutoHost tunnel closed."), false);
        return 1;
    }

    static boolean stopTunnel() {
        TunnelClient client = ACTIVE_TUNNEL.getAndSet(null);
        if (client == null) {
            return false;
        }
        client.close();
        return true;
    }
}