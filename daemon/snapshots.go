package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxRetainedSnapshots     = 4
	maxRetainedSnapshotBytes = int64(4 << 30)
)

type worldSnapshot struct {
	ID        string      `json:"id"`
	CreatedAt time.Time   `json:"createdAt"`
	Files     []worldFile `json:"files"`
	Bytes     int64       `json:"bytes"`
}

func (s *worldStore) snapshotsDir() string { return filepath.Join(s.dataDir, "backups", "snapshots") }
func (s *worldStore) blobsDir() string     { return filepath.Join(s.dataDir, "backups", "blobs") }

func (s *worldStore) backupBytesOnDisk() (int64, error) {
	blobs, err := os.ReadDir(s.blobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, blob := range blobs {
		if blob.IsDir() {
			continue
		}
		info, err := blob.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}
func (s *worldStore) createSnapshot() (worldSnapshot, error) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.worldMu.RLock()
	defer s.worldMu.RUnlock()
	files, err := s.manifest()
	if err != nil {
		return worldSnapshot{}, err
	}
	if err := os.MkdirAll(s.blobsDir(), 0o750); err != nil {
		return worldSnapshot{}, err
	}
	if err := os.MkdirAll(s.snapshotsDir(), 0o750); err != nil {
		return worldSnapshot{}, err
	}
	for _, file := range files {
		blob := filepath.Join(s.blobsDir(), file.SHA256)
		if _, err := os.Stat(blob); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return worldSnapshot{}, err
		}
		source, err := s.safePath(file.Path)
		if err != nil {
			return worldSnapshot{}, err
		}
		input, err := os.Open(source)
		if err != nil {
			return worldSnapshot{}, err
		}
		temp, err := os.CreateTemp(s.blobsDir(), ".blob-*")
		if err != nil {
			_ = input.Close()
			return worldSnapshot{}, err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(temp, hash), input)
		inputCloseErr := input.Close()
		outputSyncErr := temp.Sync()
		outputCloseErr := temp.Close()
		if copyErr != nil || inputCloseErr != nil || outputSyncErr != nil || outputCloseErr != nil {
			_ = os.Remove(temp.Name())
			return worldSnapshot{}, errors.Join(copyErr, inputCloseErr, outputSyncErr, outputCloseErr)
		}
		if written != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			_ = os.Remove(temp.Name())
			return worldSnapshot{}, fmt.Errorf("world file changed while snapshotting: %s", file.Path)
		}
		if err := os.Rename(temp.Name(), blob); err != nil {
			_ = os.Remove(temp.Name())
			if _, statErr := os.Stat(blob); statErr != nil {
				return worldSnapshot{}, err
			}
		}
	}
	snapshot := worldSnapshot{ID: time.Now().UTC().Format("20060102T150405.000000000Z"), CreatedAt: time.Now().UTC(), Files: files}
	for _, file := range files {
		snapshot.Bytes += file.Size
	}
	content, err := json.Marshal(snapshot)
	if err != nil {
		return worldSnapshot{}, err
	}
	temp, err := os.CreateTemp(s.snapshotsDir(), ".snapshot-*.tmp")
	if err != nil {
		return worldSnapshot{}, err
	}
	tempName := temp.Name()
	_, writeErr := temp.Write(content)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tempName)
		return worldSnapshot{}, errors.Join(writeErr, closeErr)
	}
	if err := os.Rename(tempName, filepath.Join(s.snapshotsDir(), snapshot.ID+".json")); err != nil {
		_ = os.Remove(tempName)
		return worldSnapshot{}, err
	}
	if err := s.pruneSnapshots(); err != nil {
		return worldSnapshot{}, err
	}
	retainedSnapshots, err := s.listSnapshots()
	if err != nil {
		return worldSnapshot{}, err
	}
	retained := false
	for _, retainedSnapshot := range retainedSnapshots {
		if retainedSnapshot.ID == snapshot.ID {
			retained = true
			break
		}
	}
	if !retained {
		return worldSnapshot{}, fmt.Errorf("snapshot exceeds the %d GiB backup limit", maxRetainedSnapshotBytes>>30)
	}
	s.addLog("world-snapshot", snapshot.ID)
	return snapshot, nil
}

func (s *worldStore) listSnapshots() ([]worldSnapshot, error) {
	entries, err := os.ReadDir(s.snapshotsDir())
	if errors.Is(err, os.ErrNotExist) {
		return []worldSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	snapshots := make([]worldSnapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(s.snapshotsDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		var snapshot worldSnapshot
		if err := json.Unmarshal(content, &snapshot); err != nil {
			continue
		}
		if snapshot.ID == strings.TrimSuffix(entry.Name(), ".json") {
			snapshots = append(snapshots, snapshot)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt) })
	return snapshots, nil
}

func (s *worldStore) restoreSnapshot(id string) error {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return errors.New("invalid snapshot ID")
	}
	content, err := os.ReadFile(filepath.Join(s.snapshotsDir(), id+".json"))
	if err != nil {
		return err
	}
	var snapshot worldSnapshot
	if err := json.Unmarshal(content, &snapshot); err != nil || snapshot.ID != id {
		return errors.New("invalid snapshot metadata")
	}
	staging, err := os.MkdirTemp(filepath.Dir(s.root), ".autohost-snapshot-restore-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	for _, file := range snapshot.Files {
		if _, err := s.safePath(file.Path); err != nil {
			return err
		}
		blob := filepath.Join(s.blobsDir(), file.SHA256)
		if len(file.SHA256) != sha256.Size*2 {
			return errors.New("invalid snapshot hash")
		}
		target := filepath.Join(staging, filepath.FromSlash(file.Path))
		if !strings.HasPrefix(target, staging+string(filepath.Separator)) {
			return errors.New("invalid snapshot path")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		input, err := os.Open(blob)
		if err != nil {
			return err
		}
		output, err := os.Create(target)
		if err != nil {
			_ = input.Close()
			return err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), input)
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil || inputCloseErr != nil || outputCloseErr != nil {
			return errors.Join(copyErr, inputCloseErr, outputCloseErr)
		}
		if written != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return errors.New("snapshot blob failed integrity check")
		}
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
	_ = os.RemoveAll(previous)
	s.invalidateCache()
	s.addLog("world-snapshot-restore", id)
	return nil
}

func (s *worldStore) pruneSnapshots() error {
	return s.pruneSnapshotsWithLimit(maxRetainedSnapshotBytes)
}

func (s *worldStore) pruneSnapshotsWithLimit(maxBytes int64) error {
	snapshots, err := s.listSnapshots()
	if err != nil {
		return err
	}
	retained := make(map[string]struct{})
	var retainedBytes int64
	retainedCount := 0
	for _, snapshot := range snapshots {
		newBlobs := make(map[string]int64)
		var additionalBytes int64
		for _, file := range snapshot.Files {
			if _, exists := retained[file.SHA256]; exists {
				continue
			}
			if _, exists := newBlobs[file.SHA256]; !exists {
				newBlobs[file.SHA256] = file.Size
				additionalBytes += file.Size
			}
		}
		if retainedCount >= maxRetainedSnapshots || retainedBytes+additionalBytes > maxBytes {
			if err := os.Remove(filepath.Join(s.snapshotsDir(), snapshot.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		retainedCount++
		for hash, size := range newBlobs {
			retained[hash] = struct{}{}
			retainedBytes += size
		}
	}
	blobs, err := os.ReadDir(s.blobsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, blob := range blobs {
		if blob.IsDir() {
			continue
		}
		if _, keep := retained[blob.Name()]; !keep {
			if err := os.Remove(filepath.Join(s.blobsDir(), blob.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *worldStore) invalidateCache() {
	s.cacheMu.Lock()
	s.cache = make(map[string]cachedWorldFile)
	_ = os.Remove(s.cachePath())
	s.cacheMu.Unlock()
}