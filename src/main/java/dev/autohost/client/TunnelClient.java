package dev.autohost.client;

import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.logging.Level;
import java.util.logging.Logger;

final class TunnelClient implements Runnable, AutoCloseable {
    private static final Logger LOGGER = Logger.getLogger(TunnelClient.class.getName());
    private static final byte[] MAGIC = "AHOST1".getBytes(StandardCharsets.US_ASCII);
    private static final int OPEN = 1;
    private static final int DATA = 2;
    private static final int CLOSE = 3;
    private static final int MAX_FRAME_BYTES = 64 * 1024;

    private final String address;
    private final int lanPort;
    private final AtomicBoolean closed = new AtomicBoolean();
    private final Map<Integer, Socket> streams = new ConcurrentHashMap<>();
    private final Object writeLock = new Object();
    private volatile Socket tunnelSocket;
    private volatile Thread runner;
    private DataInputStream input;
    private DataOutputStream output;

    TunnelClient(String address, int lanPort) {
        this.address = address;
        this.lanPort = lanPort;
    }

    @Override
    public void run() {
        runner = Thread.currentThread();
        InetSocketAddress endpoint;
        try {
            endpoint = parseAddress(address);
        } catch (IOException exception) {
            LOGGER.log(Level.WARNING, "Invalid AutoHost tunnel configuration", exception);
            close();
            return;
        }
        long retryMillis = 2_000;
        while (!closed.get()) {
            try {
                connectAndRead(endpoint);
                retryMillis = 2_000;
            } catch (IOException exception) {
                if (!closed.get()) {
                    LOGGER.log(Level.WARNING, "AutoHost tunnel disconnected; retrying", exception);
                }
            } finally {
                disconnectCurrentTunnel();
            }
            if (closed.get()) break;
            try {
                Thread.sleep(retryMillis);
            } catch (InterruptedException exception) {
                if (closed.get()) break;
                Thread.currentThread().interrupt();
                break;
            }
            retryMillis = Math.min(retryMillis * 2, 30_000);
        }
        close();
    }

    private void connectAndRead(InetSocketAddress endpoint) throws IOException {
            Socket socket = new Socket();
            tunnelSocket = socket;
            socket.connect(endpoint, 10_000);
            socket.setTcpNoDelay(true);
            input = new DataInputStream(socket.getInputStream());
            output = new DataOutputStream(socket.getOutputStream());
            output.write(MAGIC);
            output.flush();

            while (!closed.get()) {
                int type = input.readUnsignedByte();
                int streamId = input.readInt();
                int length = input.readInt();
                if (length < 0 || length > MAX_FRAME_BYTES) {
                    throw new IOException("Invalid AutoHost frame length: " + length);
                }
                byte[] payload = input.readNBytes(length);
                if (payload.length != length) {
                    throw new IOException("Truncated AutoHost frame");
                }
                handleFrame(type, streamId, payload);
            }
    }

    private void disconnectCurrentTunnel() {
        Socket socket = tunnelSocket;
        tunnelSocket = null;
        closeQuietly(socket);
        streams.values().forEach(TunnelClient::closeQuietly);
        streams.clear();
        synchronized (writeLock) {
            input = null;
            output = null;
        }
    }

    private void handleFrame(int type, int streamId, byte[] payload) throws IOException {
        if (type == OPEN) {
            openLocalStream(streamId);
            return;
        }
        Socket local = streams.get(streamId);
        if (local == null) {
            return;
        }
        if (type == DATA) {
            local.getOutputStream().write(payload);
            local.getOutputStream().flush();
        } else if (type == CLOSE) {
            closeStream(streamId, local);
        } else {
            throw new IOException("Unknown AutoHost frame type: " + type);
        }
    }

    private void openLocalStream(int streamId) throws IOException {
        Socket local = new Socket();
        try {
            local.connect(new InetSocketAddress("127.0.0.1", lanPort), 5000);
            local.setTcpNoDelay(true);
            Socket previous = streams.putIfAbsent(streamId, local);
            if (previous != null) {
                throw new IOException("Duplicate AutoHost stream: " + streamId);
            }
            Thread reader = new Thread(() -> forwardLocalToTunnel(streamId, local), "autohost-stream-" + streamId);
            reader.setDaemon(true);
            reader.start();
        } catch (IOException exception) {
            closeQuietly(local);
            sendFrame(CLOSE, streamId, new byte[0]);
            LOGGER.log(Level.WARNING, "Could not connect stream to local LAN port " + lanPort, exception);
        }
    }

    private void forwardLocalToTunnel(int streamId, Socket local) {
        byte[] buffer = new byte[MAX_FRAME_BYTES];
        try {
            int count;
            while (!closed.get() && (count = local.getInputStream().read(buffer)) >= 0) {
                if (count > 0) {
                    sendFrame(DATA, streamId, Arrays.copyOf(buffer, count));
                }
            }
        } catch (IOException exception) {
            if (!closed.get()) {
                LOGGER.log(Level.FINE, "Local Minecraft stream ended", exception);
            }
        } finally {
            if (streams.remove(streamId, local)) {
                closeQuietly(local);
                try {
                    sendFrame(CLOSE, streamId, new byte[0]);
                } catch (IOException exception) {
                    LOGGER.log(Level.FINE, "Could not close AutoHost stream", exception);
                }
            }
        }
    }

    private void sendFrame(int type, int streamId, byte[] payload) throws IOException {
        if (payload.length > MAX_FRAME_BYTES) {
            throw new IOException("AutoHost payload exceeds frame limit");
        }
        synchronized (writeLock) {
            if (closed.get() || output == null) {
                throw new IOException("AutoHost tunnel is closed");
            }
            output.writeByte(type);
            output.writeInt(streamId);
            output.writeInt(payload.length);
            output.write(payload);
            output.flush();
        }
    }

    private static InetSocketAddress parseAddress(String value) throws IOException {
        int separator = value.lastIndexOf(':');
        if (separator < 1 || separator == value.length() - 1) {
            throw new IOException("Use host:port for the Raspberry Pi address");
        }
        String host = value.substring(0, separator);
        if (host.startsWith("[") && host.endsWith("]")) {
            host = host.substring(1, host.length() - 1);
        }
        try {
            int port = Integer.parseInt(value.substring(separator + 1));
            if (port < 1 || port > 65535) {
                throw new NumberFormatException("port out of range");
            }
            return new InetSocketAddress(host, port);
        } catch (NumberFormatException exception) {
            throw new IOException("Invalid port in Raspberry Pi address", exception);
        }
    }

    private void closeStream(int streamId, Socket local) {
        if (streams.remove(streamId, local)) {
            closeQuietly(local);
        }
    }

    private static void closeQuietly(Socket socket) {
        try {
            socket.close();
        } catch (IOException ignored) {
        }
    }

    @Override
    public void close() {
        if (!closed.compareAndSet(false, true)) {
            return;
        }
        closeQuietly(tunnelSocket);
        streams.values().forEach(TunnelClient::closeQuietly);
        streams.clear();
        Thread activeRunner = runner;
        if (activeRunner != null && activeRunner != Thread.currentThread()) {
            activeRunner.interrupt();
        }
    }
}