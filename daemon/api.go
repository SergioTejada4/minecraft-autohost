package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed admin.html
var adminHTML []byte

func (d *daemon) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(adminHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /api/status", d.status)
	mux.HandleFunc("POST /api/host/claim", d.claimHost)
	mux.HandleFunc("POST /api/host/release", d.releaseHostClaim)
	mux.HandleFunc("GET /api/world/manifest", d.worldManifest)
	mux.Handle("/api/world/files/", http.HandlerFunc(d.worldFile))
	mux.HandleFunc("GET /api/admin/settings", d.adminSettings)
	mux.HandleFunc("PUT /api/admin/settings", d.adminSettings)
	mux.HandleFunc("GET /api/admin/server-icon", d.serverIcon)
	mux.HandleFunc("POST /api/admin/server-icon", d.serverIcon)
	mux.HandleFunc("GET /api/admin/logs", d.adminLogs)
	mux.HandleFunc("PUT /api/host/players", d.updateHostPlayers)
	mux.HandleFunc("GET /api/admin/world/info", d.worldInfo)
	mux.HandleFunc("GET /api/admin/world.zip", d.worldZip)
	mux.HandleFunc("POST /api/admin/world/restore", d.restoreWorld)
	mux.HandleFunc("GET /api/admin/snapshots", d.listSnapshots)
	mux.HandleFunc("POST /api/admin/snapshots", d.createSnapshot)
	mux.HandleFunc("POST /api/admin/snapshots/{id}/restore", d.restoreSnapshot)
	return mux
}

func (d *daemon) worldManifest(w http.ResponseWriter, _ *http.Request) {
	files, err := d.store.manifest()
	if err != nil {
		http.Error(w, "could not scan world", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
}

func (d *daemon) worldFile(w http.ResponseWriter, r *http.Request) {
	requestPath := strings.TrimPrefix(r.URL.Path, "/api/world/files/")
	target, err := d.store.safePath(requestPath)
	if err != nil {
		http.Error(w, "invalid world path", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		http.ServeFile(w, r, target)
	case http.MethodPut:
		if r.ContentLength > maxWorldFileBytes {
			http.Error(w, "world file exceeds 8 GiB limit", http.StatusRequestEntityTooLarge)
			return
		}
		if err := d.store.putFile(requestPath, r.Body); err != nil {
			http.Error(w, "could not store world file", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := d.store.deleteFile(requestPath); err != nil {
			http.Error(w, "could not delete world file", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *daemon) worldInfo(w http.ResponseWriter, _ *http.Request) {
	files, err := d.store.manifest()
	if err != nil {
		http.Error(w, "could not scan world", http.StatusInternalServerError)
		return
	}
	var bytes int64
	for _, file := range files {
		bytes += file.Size
	}
	snapshots, err := d.store.listSnapshots()
	if err != nil {
		http.Error(w, "could not scan backup storage", http.StatusInternalServerError)
		return
	}
	backupBytes, err := d.store.backupBytesOnDisk()
	if err != nil {
		http.Error(w, "could not inspect backup storage", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"files": len(files), "bytes": bytes, "backupBytes": backupBytes, "snapshots": len(snapshots),
		"snapshotLimit": maxRetainedSnapshots, "backupLimitBytes": maxRetainedSnapshotBytes,
	})
}

func (d *daemon) worldZip(w http.ResponseWriter, _ *http.Request) {
	if d.currentHost() != nil {
		http.Error(w, "stop the active host before downloading a world backup", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=autohost-world.zip")
	if err := d.store.zip(w); err != nil {
		return
	}
}

func (d *daemon) restoreWorld(w http.ResponseWriter, r *http.Request) {
	if d.currentHost() != nil {
		http.Error(w, "stop the active host before restoring a world", http.StatusConflict)
		return
	}
	if r.ContentLength > 8<<30 {
		http.Error(w, "archive exceeds 8 GiB upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	temp, err := os.CreateTemp(d.store.dataDir, ".autohost-upload-*.zip")
	if err != nil {
		http.Error(w, "could not create temporary upload", http.StatusInternalServerError)
		return
	}
	defer os.Remove(temp.Name())
	written, err := io.Copy(temp, io.LimitReader(r.Body, (8<<30)+1))
	closeErr := temp.Close()
	if err != nil || closeErr != nil || written > 8<<30 {
		http.Error(w, "invalid or oversized archive", http.StatusBadRequest)
		return
	}
	if err := d.store.restoreZip(temp.Name()); err != nil {
		http.Error(w, "could not restore archive", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) adminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d.store.getSettings())
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var settings adminSettings
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		http.Error(w, "invalid settings JSON", http.StatusBadRequest)
		return
	}
	if err := d.store.updateSettings(settings); err != nil {
		http.Error(w, "invalid settings", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) adminLogs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d.store.recentLogs())
}

func (d *daemon) listSnapshots(w http.ResponseWriter, _ *http.Request) {
	snapshots, err := d.store.listSnapshots()
	if err != nil {
		http.Error(w, "could not list world snapshots", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshots)
}

func (d *daemon) createSnapshot(w http.ResponseWriter, _ *http.Request) {
	snapshot, err := d.store.createSnapshot()
	if err != nil {
		http.Error(w, "could not create world snapshot", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(snapshot)
}

func (d *daemon) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	if d.currentHost() != nil {
		http.Error(w, "stop the active host before restoring a snapshot", http.StatusConflict)
		return
	}
	if err := d.store.restoreSnapshot(r.PathValue("id")); err != nil {
		http.Error(w, "could not restore snapshot", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) updateHostPlayers(w http.ResponseWriter, r *http.Request) {
	if d.currentHost() == nil {
		http.Error(w, "no active host", http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var request struct {
		Players  []string `json:"players"`
		HostName string   `json:"hostName"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.Players) > 1000 {
		http.Error(w, "invalid player list", http.StatusBadRequest)
		return
	}
	seen := make(map[string]struct{}, len(request.Players))
	players := make([]string, 0, len(request.Players))
	for _, player := range request.Players {
		player = strings.TrimSpace(player)
		if player == "" || len(player) > 32 || strings.ContainsAny(player, "\r\n") {
			http.Error(w, "invalid player name", http.StatusBadRequest)
			return
		}
		key := strings.ToLower(player)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			players = append(players, player)
		}
	}
	request.HostName = strings.TrimSpace(request.HostName)
	if len(request.HostName) > 32 || strings.ContainsAny(request.HostName, "\r\n") {
		http.Error(w, "invalid host name", http.StatusBadRequest)
		return
	}
	d.playersMu.Lock()
	previousPlayers := append([]string(nil), d.players...)
	d.players = players
	d.hostName = request.HostName
	d.playersMu.Unlock()
	if d.store != nil {
		previous := make(map[string]string, len(previousPlayers))
		current := make(map[string]struct{}, len(players))
		for _, player := range previousPlayers {
			previous[strings.ToLower(player)] = player
		}
		for _, player := range players {
			key := strings.ToLower(player)
			current[key] = struct{}{}
			if _, existed := previous[key]; !existed {
				d.store.addLog("player-connected", fmt.Sprintf("%s joined. Host: %s", player, request.HostName))
			}
		}
		for key, player := range previous {
			if _, remains := current[key]; !remains {
				d.store.addLog("player-disconnected", fmt.Sprintf("%s left. Host: %s", player, request.HostName))
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) claimHost(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.host != nil {
		http.Error(w, "host already active", http.StatusConflict)
		return
	}
	owner, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		owner = r.RemoteAddr
	}
	newClaim := !d.claimUntil.After(time.Now())
	if !newClaim {
		if d.claimOwner != owner {
			http.Error(w, "another player is becoming host", http.StatusConflict)
			return
		}
	} else {
		d.claimOwner = owner
	}
	d.claimUntil = time.Now().Add(2 * time.Minute)
	if newClaim && d.store != nil {
		d.store.addLog("host-claim", "a player is preparing to host")
	}
	w.WriteHeader(http.StatusCreated)
}

func (d *daemon) releaseHostClaim(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	if d.host == nil {
		d.claimUntil = time.Time{}
		d.claimOwner = ""
	}
	d.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}