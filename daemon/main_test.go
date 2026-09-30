package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	want := frame{typ: frameData, stream: 42, payload: []byte("minecraft traffic")}
	var buffer bytes.Buffer
	if err := writeFrame(&buffer, want); err != nil {
		t.Fatal(err)
	}
	got, err := readFrame(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got.typ != want.typ || got.stream != want.stream || !reflect.DeepEqual(got.payload, want.payload) {
		t.Fatalf("readFrame() = %#v, want %#v", got, want)
	}
}

func TestReadFrameRejectsOversizedPayload(t *testing.T) {
	buffer := bytes.NewBuffer([]byte{frameData, 0, 0, 0, 1, 0, 1, 0, 1})
	if _, err := readFrame(buffer); err == nil {
		t.Fatal("readFrame() accepted an oversized payload")
	}
}

func TestTunnelProxiesBothDirections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	d := &daemon{}
	go d.acceptTCP(listener)

	host, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(host, tunnelMagic); err != nil {
		t.Fatal(err)
	}

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	const initial = "CLIENT"
	if _, err := io.WriteString(client, playerMagic); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, initial); err != nil {
		t.Fatal(err)
	}

	open, err := readFrame(host)
	if err != nil {
		t.Fatal(err)
	}
	if open.typ != frameOpen {
		t.Fatalf("first host frame type = %d, want OPEN", open.typ)
	}
	fromClient, err := readFrame(host)
	if err != nil {
		t.Fatal(err)
	}
	if fromClient.typ != frameData || string(fromClient.payload) != initial {
		t.Fatalf("host received frame %#v, want client data %q", fromClient, initial)
	}

	const response = "HOST!!"
	if err := writeFrame(host, frame{typ: frameData, stream: open.stream, payload: []byte(response)}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != response {
		t.Fatalf("client received %q, want %q", got, response)
	}
}

func TestWorldAPITransfersFilesWithoutAuthentication(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{store: store}
	server := httptest.NewServer(d.apiHandler())
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/world/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public manifest status = %d, want 200", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodPut, server.URL+"/api/world/files/region/r.0.0.mca", strings.NewReader("region-data"))
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("world upload status = %d, want 204", response.StatusCode)
	}

	request, err = http.NewRequest(http.MethodGet, server.URL+"/api/world/manifest", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var manifest struct {
		Files []worldFile `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "region/r.0.0.mca" || manifest.Files[0].Size != int64(len("region-data")) {
		t.Fatalf("unexpected world manifest: %#v", manifest.Files)
	}
	if _, err := os.Stat(filepath.Join(root, "world", "region", "r.0.0.mca")); err != nil {
		t.Fatal(err)
	}
}

func TestWorldStoreRejectsTraversal(t *testing.T) {
	store, err := newWorldStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.safePath("../outside"); err == nil {
		t.Fatal("safePath accepted a parent traversal")
	}
}

func TestAdminSettingsValidateAndPersistAdminPlayers(t *testing.T) {
	store, err := newWorldStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateSettings(adminSettings{MOTD: "World", AdminPlayers: []string{" Steve ", "steve", "Alex_2"}}); err != nil {
		t.Fatal(err)
	}
	admins := store.getSettings().AdminPlayers
	if !reflect.DeepEqual(admins, []string{"Steve", "Alex_2"}) {
		t.Fatalf("admin players = %#v, want normalized unique names", admins)
	}
	if err := store.updateSettings(adminSettings{AdminPlayers: []string{"not a username"}}); err == nil {
		t.Fatal("settings accepted an invalid Minecraft username as admin")
	}
}

func TestHostPlayerUpdatesLogConnectionsAndDisconnections(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{store: store, host: &tunnel{}}
	server := httptest.NewServer(d.apiHandler())
	defer server.Close()
	update := func(body string) {
		t.Helper()
		request, err := http.NewRequest(http.MethodPut, server.URL+"/api/host/players", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("player update status = %d, want 204", response.StatusCode)
		}
	}
	update(`{"players":["Alex","Sam"],"hostName":"Host"}`)
	update(`{"players":["Sam"],"hostName":"Host"}`)
	events := store.recentLogs()
	if len(events) != 3 || events[0].Action != "player-connected" || events[1].Action != "player-connected" || events[2].Action != "player-disconnected" {
		t.Fatalf("player activity events = %#v, want two joins and one leave", events)
	}
	if !strings.Contains(events[0].Detail, "Alex") || !strings.Contains(events[2].Detail, "Alex") {
		t.Fatalf("player activity details do not name Alex: %#v", events)
	}
}

func TestHostDisconnectLogsRemainingPlayers(t *testing.T) {
	store, err := newWorldStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := &tunnel{streams: make(map[uint32]net.Conn)}
	d := &daemon{store: store, host: host, players: []string{"Alex", "Host"}, hostName: "Host"}
	d.removeHost(host)
	events := store.recentLogs()
	if len(events) != 3 || events[0].Action != "player-disconnected" || events[1].Action != "player-disconnected" || events[2].Action != "host-disconnected" {
		t.Fatalf("host disconnect events = %#v, want player disconnects followed by host disconnect", events)
	}
	d.playersMu.RLock()
	hostName := d.hostName
	playerCount := len(d.players)
	d.playersMu.RUnlock()
	if hostName != "" || playerCount != 0 {
		t.Fatalf("host state after disconnect = name %q, players %d; want empty", hostName, playerCount)
	}
}

func TestActiveHostStatusPingUsesPiSettingsAndLivePlayerCount(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateSettings(adminSettings{MOTD: "Pi world asleep"}); err != nil {
		t.Fatal(err)
	}
	d := &daemon{store: store, host: &tunnel{}, players: []string{"Alex", "Sam"}}
	if err := os.WriteFile(d.iconPath(), []byte("configured-icon"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	clientSide, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientSide.Close()
	_ = clientSide.SetDeadline(time.Now().Add(10 * time.Second))
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		d.acceptTCP(listener)
	}()

	var handshake bytes.Buffer
	writeVarInt(&handshake, 0)
	writeVarInt(&handshake, 767)
	writeVarInt(&handshake, int32(len("localhost")))
	_, _ = handshake.WriteString("localhost")
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], 25565)
	_, _ = handshake.Write(port[:])
	writeVarInt(&handshake, 1)
	if err := writePacket(clientSide, handshake.Bytes()); err != nil {
		t.Fatal(err)
	}
	statusRequestDone := make(chan error, 1)
	go func() { statusRequestDone <- writePacket(clientSide, []byte{0}) }()
	packetID, payload, err := newProtocolReader(clientSide).packet()
	if err != nil {
		t.Fatal(err)
	}
	if packetID != 0 {
		t.Fatalf("status response packet ID = %d, want 0", packetID)
	}
	if err := <-statusRequestDone; err != nil {
		t.Fatal(err)
	}
	statusPayload := bytes.NewReader(payload)
	jsonLength, err := readVarInt(statusPayload)
	if err != nil {
		t.Fatal(err)
	}
	jsonBytes := make([]byte, jsonLength)
	if _, err := io.ReadFull(statusPayload, jsonBytes); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Description struct {
			Text string `json:"text"`
		} `json:"description"`
		Players struct {
			Online int `json:"online"`
		} `json:"players"`
		Favicon string `json:"favicon"`
	}
	if err := json.Unmarshal(jsonBytes, &status); err != nil {
		t.Fatal(err)
	}
	if status.Description.Text != "Pi world asleep" {
		t.Fatalf("status MOTD = %q", status.Description.Text)
	}
	if status.Players.Online != 2 {
		t.Fatalf("status online players = %d, want 2", status.Players.Online)
	}
	if status.Favicon != "data:image/png;base64,Y29uZmlndXJlZC1pY29u" {
		t.Fatalf("status favicon = %q, want configured Pi icon", status.Favicon)
	}
	select {
	case <-serverDone:
		t.Fatal("Minecraft status handler closed before receiving the ping packet")
	default:
	}
	var ping bytes.Buffer
	writeVarInt(&ping, 1)
	_ = binary.Write(&ping, binary.BigEndian, int64(1234))
	pingRequestDone := make(chan error, 1)
	go func() { pingRequestDone <- writePacket(clientSide, ping.Bytes()) }()
	pongID, pong, err := newProtocolReader(clientSide).packet()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-pingRequestDone; err != nil {
		t.Fatal(err)
	}
	if pongID != 1 || !bytes.Equal(pong, ping.Bytes()[1:]) {
		t.Fatalf("invalid pong packet: ID %d, payload %x", pongID, pong)
	}
}

func TestSnapshotRestoresPriorWorldVersion(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.putFile("level.dat", strings.NewReader("first world")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.createSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.putFile("level.dat", strings.NewReader("second world")); err != nil {
		t.Fatal(err)
	}
	if err := store.restoreSnapshot(snapshot.ID); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "world", "level.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first world" {
		t.Fatalf("restored level.dat = %q, want original snapshot", content)
	}
	if _, err := store.listSnapshots(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRetentionLimitsCountAndBlobBytes(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"one", "two", "three", "four", "five"} {
		if err := store.putFile("level.dat", strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.createSnapshot(); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := store.listSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != maxRetainedSnapshots {
		t.Fatalf("retained snapshot count = %d, want %d", len(snapshots), maxRetainedSnapshots)
	}

	quotaRoot := t.TempDir()
	quotaStore, err := newWorldStore(filepath.Join(quotaRoot, "world"), filepath.Join(quotaRoot, "data"))
	if err != nil {
		t.Fatal(err)
	}
	var created []worldSnapshot
	for _, content := range []string{"a", "bb", "ccc"} {
		if err := quotaStore.putFile("level.dat", strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		snapshot, err := quotaStore.createSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, snapshot)
	}
	if err := quotaStore.pruneSnapshotsWithLimit(5); err != nil {
		t.Fatal(err)
	}
	snapshots, err = quotaStore.listSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].ID != created[2].ID || snapshots[1].ID != created[1].ID {
		t.Fatalf("snapshots retained under 5-byte quota = %#v, want newest 3- and 2-byte versions", snapshots)
	}
	backupBytes, err := quotaStore.backupBytesOnDisk()
	if err != nil {
		t.Fatal(err)
	}
	if backupBytes != 5 {
		t.Fatalf("snapshot blob usage = %d, want 5 bytes", backupBytes)
	}
}

func TestWorldZipRoundTripsThroughRestoreAPI(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"level.dat": "saved level", "region/r.0.0.mca": "saved region"} {
		if err := store.putFile(path, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer((&daemon{store: store}).apiHandler())
	defer server.Close()
	zipResponse, err := http.Get(server.URL + "/api/admin/world.zip")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(zipResponse.Body)
	_ = zipResponse.Body.Close()
	if err != nil || zipResponse.StatusCode != http.StatusOK {
		t.Fatalf("world ZIP download failed: status=%d error=%v", zipResponse.StatusCode, err)
	}
	if err := store.putFile("level.dat", strings.NewReader("incorrect local world")); err != nil {
		t.Fatal(err)
	}
	if err := store.putFile("old-only.dat", strings.NewReader("must be removed")); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/admin/world/restore", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/zip")
	restoreResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = restoreResponse.Body.Close()
	if restoreResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("world ZIP restore status = %d, want 204", restoreResponse.StatusCode)
	}
	for path, want := range map[string]string{"level.dat": "saved level", "region/r.0.0.mca": "saved region"} {
		content, err := os.ReadFile(filepath.Join(store.root, filepath.FromSlash(path)))
		if err != nil || string(content) != want {
			t.Fatalf("restored %s = %q, error %v; want %q", path, content, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(store.root, "old-only.dat")); !os.IsNotExist(err) {
		t.Fatalf("old-only file still exists after restore: %v", err)
	}
}

func TestRestoreZipWithoutRootLevelDatPreservesWorld(t *testing.T) {
	root := t.TempDir()
	store, err := newWorldStore(filepath.Join(root, "world"), filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.putFile("level.dat", strings.NewReader("keep this world")); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create("not-a-world.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "not a world"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(root, "invalid.zip")
	if err := os.WriteFile(zipPath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.restoreZip(zipPath); err == nil {
		t.Fatal("restoreZip accepted an archive without root level.dat")
	}
	content, err := os.ReadFile(filepath.Join(store.root, "level.dat"))
	if err != nil || string(content) != "keep this world" {
		t.Fatalf("existing world after rejected restore = %q, error %v", content, err)
	}
}