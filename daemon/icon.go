package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const maxIconUploadBytes = 8 << 20

func (d *daemon) iconPath() string {
	return filepath.Join(d.store.dataDir, "server-icon.png")
}

func (d *daemon) serverIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "image/png")
		http.ServeFile(w, r, d.iconPath())
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > maxIconUploadBytes {
		http.Error(w, "icon upload exceeds 8 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, maxIconUploadBytes+1))
	if err != nil || len(content) > maxIconUploadBytes {
		http.Error(w, "could not read icon upload", http.StatusBadRequest)
		return
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || (format != "png" && format != "jpeg") || config.Width < 1 || config.Height < 1 ||
		config.Width > 2048 || config.Height > 2048 || int64(config.Width)*int64(config.Height) > 4_194_304 {
		http.Error(w, "upload a PNG or JPEG up to 2048x2048", http.StatusBadRequest)
		return
	}
	source, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		http.Error(w, "invalid image", http.StatusBadRequest)
		return
	}
	icon := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			sourceX := source.Bounds().Min.X + x*source.Bounds().Dx()/64
			sourceY := source.Bounds().Min.Y + y*source.Bounds().Dy()/64
			icon.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, icon); err != nil {
		http.Error(w, "could not resize icon", http.StatusInternalServerError)
		return
	}
	temp, err := os.CreateTemp(d.store.dataDir, ".server-icon-*.png")
	if err != nil {
		http.Error(w, "could not store icon", http.StatusInternalServerError)
		return
	}
	_, writeErr := temp.Write(output.Bytes())
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(temp.Name())
		http.Error(w, "could not store icon", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(temp.Name(), d.iconPath()); err != nil {
		_ = os.Remove(temp.Name())
		http.Error(w, "could not store icon", http.StatusInternalServerError)
		return
	}
	d.store.addLog("server-icon", "server icon updated")
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) faviconDataURL() (string, error) {
	content, err := os.ReadFile(d.iconPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(content), nil
}

