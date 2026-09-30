package dev.autohost.client;

import net.minecraft.client.Minecraft;
import net.minecraft.client.gui.screens.Screen;
import net.minecraft.client.gui.screens.ConnectScreen;
import net.minecraft.client.multiplayer.ServerData;
import net.minecraft.client.multiplayer.resolver.ServerAddress;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicReference;
import java.util.logging.Level;
import java.util.logging.Logger;

final class PlayerConnectionProxy implements AutoCloseable {
    static final byte[] PLAYER_MAGIC = "AHPLAY".getBytes(StandardCharsets.US_ASCII);
    private static final Logger LOGGER = Logger.getLogger(PlayerConnectionProxy.class.getName());
    private static final AtomicReference<PlayerConnectionProxy> ACTIVE = new AtomicReference<>();

    private final AutoHostClientConfig config;
    private final ServerSocket listener;
    private final AtomicBoolean closed = new AtomicBoolean();
    private volatile Socket localSocket;
    private volatile Socket remoteSocket;

    private PlayerConnectionProxy(AutoHostClientConfig config) throws IOException {
        this.config = config;
        listener = new ServerSocket();
        listener.setReuseAddress(true);
        listener.bind(new InetSocketAddress(InetAddress.getByAddress(new byte[]{127, 0, 0, 1}), 0));
    }

    static void connect(Screen parent, Minecraft minecraft, ServerData serverData,
                        AutoHostClientConfig config) throws IOException {
        PlayerConnectionProxy previous = ACTIVE.getAndSet(null);
        if (previous != null) previous.close();
        PlayerConnectionProxy proxy = new PlayerConnectionProxy(config);
        ACTIVE.set(proxy);
        Thread thread = new Thread(proxy::acceptAndForward, "autohost-player-proxy");
        thread.setDaemon(true);
        thread.start();
        String localAddress = "127.0.0.1:" + proxy.listener.getLocalPort();
        ConnectScreen.startConnecting(parent, minecraft, ServerAddress.parseString(localAddress),
                serverData, false, null);
    }

    private void acceptAndForward() {
        try {
            localSocket = listener.accept();
            remoteSocket = new Socket();
            remoteSocket.connect(new InetSocketAddress(config.serverIp(), 25565), 10_000);
            remoteSocket.setTcpNoDelay(true);
            OutputStream remoteOutput = remoteSocket.getOutputStream();
            remoteOutput.write(PLAYER_MAGIC);
            remoteOutput.flush();
            listener.close();
            Thread outbound = new Thread(() -> copy(localSocket, remoteSocket), "autohost-player-upstream");
            outbound.setDaemon(true);
            outbound.start();
            copy(remoteSocket, localSocket);
        } catch (IOException exception) {
            if (!closed.get()) LOGGER.log(Level.FINE, "AutoHost player connection ended", exception);
        } finally {
            close();
            ACTIVE.compareAndSet(this, null);
        }
    }

    private void copy(Socket source, Socket destination) {
        byte[] buffer = new byte[64 * 1024];
        try {
            InputStream input = source.getInputStream();
            OutputStream output = destination.getOutputStream();
            int count;
            while (!closed.get() && (count = input.read(buffer)) >= 0) {
                if (count > 0) {
                    output.write(buffer, 0, count);
                    output.flush();
                }
            }
        } catch (IOException exception) {
            if (!closed.get()) LOGGER.log(Level.FINE, "AutoHost player proxy stream ended", exception);
        } finally {
            close();
        }
    }

    @Override
    public void close() {
        if (!closed.compareAndSet(false, true)) return;
        try {
            listener.close();
        } catch (IOException ignored) {
        }
        closeSocket(localSocket);
        closeSocket(remoteSocket);
    }

    private static void closeSocket(Socket socket) {
        if (socket == null) return;
        try {
            socket.close();
        } catch (IOException ignored) {
        }
    }
}