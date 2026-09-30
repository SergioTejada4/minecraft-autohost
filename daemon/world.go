package main

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxWorldFileBytes = 8 << 30

type worldFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type adminSettings struct {
	MOTD             string   `json:"motd"`
	WhitelistEnabled bool     `json:"whitelistEnabled"`
	AllowedPlayers   []string `json:"allowedPlayers"`
	AdminPlayers     []string `json:"adminPlayers"`
}

type activityEvent struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

type cachedWorldFile struct {
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
}

type worldStore struct {
	root     string
	dataDir  string
	mu       sync.RWMutex
	worldMu  sync.RWMutex
	settings adminSettings
	logs     []activityEvent
	cacheMu  sync.Mutex
	cache    map[string]cachedWorldFile
	snapshotMu sync.Mutex
}

func newWorldStore(root, dataDir string) (*worldStore, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	s := &worldStore{root: root, dataDir: dataDir, settings: adminSettings{MOTD: "AutoHost Minecraft"}, cache: make(map[string]cachedWorldFile)}
	if err := s.loadSettings(); err != nil {
		return nil, err
	}
	if err := s.loadLogs(); err != nil {
		return nil, err
	}
	if err := s.loadCache(); err != nil {
		return nil, err
	}
	if err := s.pruneSnapshots(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *worldStore) settingsPath() string { return filepath.Join(s.dataDir, "settings.json") }
func (s *worldStore) logPath() string      { return filepath.Join(s.dataDir, "activity.jsonl") }
func (s *worldStore) cachePath() string    { return filepath.Join(s.dataDir, "world-cache.json") }

func (s *worldStore) loadCache() error {
	content, err := os.ReadFile(s.cachePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(content, &s.cache)
}

func (s *worldStore) loadSettings() error {
	content, err := os.ReadFile(s.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return s.saveSettings()
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(content, &s.settings)
}

func (s *worldStore) getSettings() adminSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copy := s.settings
	copy.AllowedPlayers = append([]string(nil), s.settings.AllowedPlayers...)
	copy.AdminPlayers = append([]string(nil), s.settings.AdminPlayers...)
	return copy
}

func (s *worldStore) updateSettings(next adminSettings) error {
	if len(next.MOTD) > 256 || len(next.AllowedPlayers) > 10000 || len(next.AdminPlayers) > 10000 {
		return errors.New("settings exceed allowed limits")
	}
	for i, player := range next.AllowedPlayers {
		player = strings.TrimSpace(player)
		if len(player) > 64 || strings.ContainsAny(player, "\r\n") {
			return fmt.Errorf("invalid player name at index %d", i)
		}
		next.AllowedPlayers[i] = player
	}
	seenAdmins := make(map[string]struct{}, len(next.AdminPlayers))
	admins := make([]string, 0, len(next.AdminPlayers))
	for _, player := range next.AdminPlayers {
		player = strings.TrimSpace(player)
		if len(player) < 1 || len(player) > 16 {
			return errors.New("admin names must be valid Minecraft usernames")
		}
		for _, char := range player {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_') {
				return errors.New("admin names must be valid Minecraft usernames")
			}
		}
		key := strings.ToLower(player)
		if _, exists := seenAdmins[key]; !exists {
			seenAdmins[key] = struct{}{}
			admins = append(admins, player)
		}
	}
	next.AdminPlayers = admins
	s.mu.Lock()
	previous := s.settings
	s.settings = next
	err := s.saveSettingsLocked()
	if err != nil {
		s.settings = previous
	}
	s.mu.Unlock()
	if err == nil {
		s.addLog("settings", "server settings updated")
	}
	return err
}

func (s *worldStore) saveSettings() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.saveSettingsLocked()
}

func (s *worldStore) saveSettingsLocked() error {
	content, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.settingsPath(), content, 0o600)
}

func (s *worldStore) addLog(action, detail string) {
	event := activityEvent{Time: time.Now().UTC(), Action: action, Detail: detail}
	content, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.logs = append(s.logs, event)
	if len(s.logs) > 500 {
		s.logs = append([]activityEvent(nil), s.logs[len(s.logs)-500:]...)
	}
	file, openErr := os.OpenFile(s.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if openErr == nil {
		_, _ = file.Write(append(content, '\n'))
		_ = file.Close()
	}
	s.mu.Unlock()
}

func (s *worldStore) recentLogs() []activityEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]activityEvent(nil), s.logs...)
}

func (s *worldStore) safePath(requestPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(requestPath))
	if requestPath == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid world file path")
	}
	target := filepath.Join(s.root, clean)
	rel, err := filepath.Rel(s.root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid world file path")
	}
	current := s.root
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || (index < len(parts)-1 && !info.IsDir()) {
			return "", errors.New("invalid world path component")
		}
	}
	return target, nil
}

func (s *worldStore) manifest() ([]worldFile, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	nextCache := make(map[string]cachedWorldFile)
	files := make([]worldFile, 0)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Name() == "session.lock" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		modified := info.ModTime().UnixNano()
		cached, exists := s.cache[rel]
		if !exists || cached.Size != info.Size() || cached.ModTime != modified {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			cached = cachedWorldFile{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size(), ModTime: modified}
		}
		nextCache[rel] = cached
		files = append(files, worldFile{Path: rel, SHA256: cached.SHA256, Size: info.Size()})
		return nil
	})
	if err == nil {
		content, marshalErr := json.Marshal(nextCache)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if writeErr := os.WriteFile(s.cachePath(), content, 0o600); writeErr != nil {
			return nil, writeErr
		}
		s.cache = nextCache
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}

func (s *worldStore) putFile(requestPath string, body io.Reader) error {
	target, err := s.safePath(requestPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".autohost-upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	written, err := io.Copy(temp, io.LimitReader(body, maxWorldFileBytes+1))
	if err != nil {
		_ = temp.Close()
		return err
	}
	if written > maxWorldFileBytes {
		_ = temp.Close()
		return errors.New("world file exceeds 8 GiB limit")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	s.worldMu.Lock()
	renameErr := os.Rename(temp.Name(), target)
	s.worldMu.Unlock()
	if renameErr != nil {
		return renameErr
	}
	s.invalidateCachedPath(requestPath)
	s.addLog("world-upload", requestPath)
	return nil
}

func (s *worldStore) deleteFile(requestPath string) error {
	target, err := s.safePath(requestPath)
	if err != nil {
		return err
	}
	s.worldMu.Lock()
	removeErr := os.Remove(target)
	s.worldMu.Unlock()
	if removeErr != nil {
		return removeErr
	}
	s.invalidateCachedPath(requestPath)
	s.addLog("world-delete", requestPath)
	return nil
}

func (s *worldStore) zip(w http.ResponseWriter) error {
	s.worldMu.RLock()
	defer s.worldMu.RUnlock()
	archive := zip.NewWriter(w)
	walkErr := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() == "session.lock" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	closeErr := archive.Close()
	if walkErr != nil {
		return walkErr
	}
	return closeErr
}

func (s *worldStore) restoreZip(source string) error {
	archive, err := zip.OpenReader(source)
	if err != nil {
		return err
	}
	defer archive.Close()
	staging, err := os.MkdirTemp(filepath.Dir(s.root), ".autohost-restore-*")
	if err != nil {
		return err
	}
	defer func() {
		if staging != "" {
			_ = os.RemoveAll(staging)
		}
	}()
	var expanded int64
	for _, entry := range archive.File {
		if entry.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive contains a symbolic link")
		}
		if entry.UncompressedSize64 > uint64(maxWorldFileBytes) || expanded+int64(entry.UncompressedSize64) > 16<<30 {
			return errors.New("archive exceeds restore size limits")
		}
		expanded += int64(entry.UncompressedSize64)
		rel := filepath.Clean(filepath.FromSlash(entry.Name))
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("archive contains an invalid path")
		}
		target := filepath.Join(staging, rel)
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		r, err := entry.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			_ = r.Close()
			return err
		}
		written, copyErr := io.Copy(out, io.LimitReader(r, int64(entry.UncompressedSize64)+1))
		closeReadErr := r.Close()
		closeOutErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if written != int64(entry.UncompressedSize64) {
			return errors.New("archive entry size does not match its metadata")
		}
		if closeReadErr != nil {
			return closeReadErr
		}
		if closeOutErr != nil {
			return closeOutErr
		}
	}
	levelInfo, err := os.Stat(filepath.Join(staging, "level.dat"))
	if err != nil || !levelInfo.Mode().IsRegular() || levelInfo.Size() == 0 {
		return errors.New("archive does not contain a valid root level.dat")
	}
	s.worldMu.Lock()
	defer s.worldMu.Unlock()
	previous := s.root + ".previous"
	_ = os.RemoveAll(previous)
	if err := os.Rename(s.root, previous); err != nil {
		return err
	}
	if err := os.Rename(staging, s.root); err != nil {
		_ = os.Rename(previous, s.root)
		return err
	}
	staging = ""
	_ = os.RemoveAll(previous)
	s.invalidateCache()
	s.addLog("world-restore", "world restored from ZIP")
	return nil
}

func (s *worldStore) invalidateCachedPath(path string) {
	s.cacheMu.Lock()
	delete(s.cache, filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))))
	s.cacheMu.Unlock()
}

func (s *worldStore) loadLogs() error {
	file, err := os.Open(s.logPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var events []activityEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		var event activityEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			events = append(events, event)
			if len(events) > 500 {
				events = events[1:]
			}
		}
	}
	s.logs = events
	return scanner.Err()
}