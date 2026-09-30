package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	tunnelMagic   = "AHOST1"
	playerMagic   = "AHPLAY"
	frameOpen     = 1
	frameData     = 2
	frameClose    = 3
	maxFrameBytes = 64 * 1024
)

type frame struct {
	typ     byte
	stream  uint32
	payload []byte
}

func readFrame(r io.Reader) (frame, error) {
	var header [9]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}
	length := binary.BigEndian.Uint32(header[5:])
	if length > maxFrameBytes {
		return frame{}, fmt.Errorf("frame too large: %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	return frame{typ: header[0], stream: binary.BigEndian.Uint32(header[1:5]), payload: payload}, nil
}

func writeFrame(w io.Writer, f frame) error {
	if len(f.payload) > maxFrameBytes {
		return fmt.Errorf("frame too large: %d", len(f.payload))
	}
	var header [9]byte
	header[0] = f.typ
	binary.BigEndian.PutUint32(header[1:5], f.stream)
	binary.BigEndian.PutUint32(header[5:9], uint32(len(f.payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, f.payload)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

type tunnel struct {
	conn    net.Conn
	writes  sync.Mutex
	mu      sync.Mutex
	streams map[uint32]net.Conn
}

func (t *tunnel) send(f frame) error {
	t.writes.Lock()
	defer t.writes.Unlock()
	return writeFrame(t.conn, f)
}

type daemon struct {
	store     *worldStore
	mu        sync.RWMutex
	host      *tunnel
	claimUntil time.Time
	claimOwner string
	streams   atomic.Uint32
	playersMu sync.RWMutex
	players   []string
	hostName  string
}

func (d *daemon) registerHost(t *tunnel) bool {
	d.mu.Lock()
	if d.host != nil {
		d.mu.Unlock()
		return false
	}
	d.host = t
	d.claimUntil = time.Time{}
	d.claimOwner = ""
	d.mu.Unlock()
	d.playersMu.Lock()
	d.players = nil
	d.hostName = ""
	d.playersMu.Unlock()
	if d.store != nil {
		d.store.addLog("host-connected", "host tunnel connected")
	}
	log.Printf("host tunnel connected")
	return true
}

func (d *daemon) removeHost(t *tunnel) {
	d.mu.Lock()
	wasCurrent := d.host == t
	if wasCurrent {
		d.host = nil
	}
	d.mu.Unlock()
	if wasCurrent {
		d.playersMu.Lock()
		disconnectedPlayers := append([]string(nil), d.players...)
		hostName := d.hostName
		d.players = nil
		d.hostName = ""
		d.playersMu.Unlock()
		if d.store != nil {
			for _, player := range disconnectedPlayers {
				d.store.addLog("player-disconnected", fmt.Sprintf("%s left when host %s closed the world", player, hostName))
			}
		}
	}
	t.mu.Lock()
	for id, conn := range t.streams {
		_ = conn.Close()
		delete(t.streams, id)
	}
	t.mu.Unlock()
	if d.store != nil {
		d.store.addLog("host-disconnected", "host tunnel disconnected")
	}
	log.Printf("host tunnel disconnected")
}

func (d *daemon) currentHost() *tunnel {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.host
}

func (d *daemon) acceptTCP(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("TCP accept: %v", err)
			}
			return
		}
		go d.handleConnection(conn)
	}
}

func (d *daemon) handleConnection(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var prefix [len(tunnelMagic)]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		_ = conn.Close()
		return
	}
	if string(prefix[:]) == tunnelMagic {
		_ = conn.SetReadDeadline(time.Time{})
		d.serveTunnel(conn)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if string(prefix[:]) == playerMagic {
		d.proxyClient(conn, conn)
		return
	}
	reader := bufio.NewReader(io.MultiReader(bytes.NewReader(prefix[:]), conn))
	state, handshake, err := readMinecraftHandshake(reader)
	if err != nil {
		_ = conn.Close()
		return
	}
	if state == 2 {
		_ = writeLoginDisconnect(conn, "This AutoHost server requires the AutoHost P2P mod.")
		_ = conn.Close()
		return
	}
	if state != 1 {
		_ = conn.Close()
		return
	}
	d.handleMinecraftStatus(conn, io.MultiReader(bytes.NewReader(handshake), reader))
}

func (d *daemon) proxyClient(conn net.Conn, reader io.Reader) {
	t := d.currentHost()
	if t == nil {
		d.handleNoHost(conn, reader)
		return
	}
	id := d.streams.Add(1)
	t.mu.Lock()
	t.streams[id] = conn
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.streams, id)
		t.mu.Unlock()
		_ = conn.Close()
		_ = t.send(frame{typ: frameClose, stream: id})
	}()
	if err := t.send(frame{typ: frameOpen, stream: id}); err != nil {
		return
	}
	buffer := make([]byte, maxFrameBytes)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if sendErr := t.send(frame{typ: frameData, stream: id, payload: buffer[:n]}); sendErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (d *daemon) serveTunnel(conn net.Conn) {
	t := &tunnel{conn: conn, streams: make(map[uint32]net.Conn)}
	if !d.registerHost(t) {
		log.Printf("rejected competing host tunnel")
		_ = conn.Close()
		return
	}
	defer d.removeHost(t)
	reader := bufio.NewReader(conn)
	for {
		f, err := readFrame(reader)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				log.Printf("tunnel read: %v", err)
			}
			return
		}
		if f.typ != frameData && f.typ != frameClose {
			log.Printf("invalid tunnel frame type %d", f.typ)
			return
		}
		t.mu.Lock()
		stream := t.streams[f.stream]
		if f.typ == frameClose {
			delete(t.streams, f.stream)
		}
		t.mu.Unlock()
		if stream == nil {
			continue
		}
		if f.typ == frameClose {
			_ = stream.Close()
		} else if err := writeAll(stream, f.payload); err != nil {
			_ = stream.Close()
		}
	}
}

func (d *daemon) status(w http.ResponseWriter, _ *http.Request) {
	d.mu.RLock()
	active := d.host != nil
	syncing := !active && d.claimUntil.After(time.Now())
	var clients int
	if d.host != nil {
		d.host.mu.Lock()
		clients = len(d.host.streams)
		d.host.mu.Unlock()
	}
	d.mu.RUnlock()
	d.playersMu.RLock()
	playerNames := append([]string(nil), d.players...)
	hostName := d.hostName
	d.playersMu.RUnlock()
	state := "idle"
	if active {
		state = "hosting"
	} else if syncing {
		state = "syncing"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"service": "autohost-p2p", "state": state, "hostActive": active, "syncing": syncing, "hostName": hostName, "players": clients, "playerNames": playerNames})
}

func run() error {
	tcpAddr := envOr("AUTOHOST_TCP_ADDR", ":25565")
	httpAddr := envOr("AUTOHOST_HTTP_ADDR", ":8080")
	tcpListener, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		return fmt.Errorf("listen TCP %s: %w", tcpAddr, err)
	}
	dataDir := envOr("AUTOHOST_DATA_DIR", "./data")
	store, err := newWorldStore(envOr("AUTOHOST_WORLD_DIR", "./data/world"), dataDir)
	if err != nil {
		return fmt.Errorf("initialize world storage: %w", err)
	}
	d := &daemon{store: store}
	httpServer := &http.Server{Addr: httpAddr, Handler: d.apiHandler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go d.acceptTCP(tcpListener)
	go func() {
		<-ctx.Done()
		_ = tcpListener.Close()
		_ = httpServer.Shutdown(context.Background())
	}()
	log.Printf("AutoHost TCP proxy listening on %s; status API on %s", tcpAddr, httpAddr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

