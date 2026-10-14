package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Whistle (Cactus Compute, Apache-2.0) turns the user's speech into text. It runs in
// the browser on onnxruntime-web, so the app's only job is to fetch and serve its
// files: a 17 MB weight pack plus two small ONNX graphs, ~18 MB in all. The page
// expands the pack into the model's weights, so the 145 MB of weight files in the
// ONNX release are never downloaded.
//
//	runtime/models/whistle/whistle.pack
//	runtime/models/whistle/onnx/encoder.onnx
//	runtime/models/whistle/onnx/decoder.onnx
const whistleRepo = "mrfakename/whistle-ONNX"

var whistleFiles = []string{"whistle.pack", "onnx/encoder.onnx", "onnx/decoder.onnx"}

func (a *app) whistleDir() string { return filepath.Join(a.runtimeDir(), "models", "whistle") }

func (a *app) whistleReady() bool {
	for _, name := range whistleFiles {
		if !isFile(filepath.Join(a.whistleDir(), filepath.FromSlash(name))) {
			return false
		}
	}
	return true
}

// ensureWhistle downloads any missing Whistle file, verifying each against the
// repository's published checksum.
func (a *app) ensureWhistle() error {
	for _, name := range whistleFiles {
		dest := filepath.Join(a.whistleDir(), filepath.FromSlash(name))
		if isFile(dest) {
			continue
		}
		a.setStageActivity("Downloading the speech recognition model")
		if err := a.ensureModel(whistleRepo, name, dest); err != nil {
			return err
		}
	}
	return nil
}

// handleWhistleFile serves the model files to the page's speech worker. Only the
// three known files are served.
func (a *app) handleWhistleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/models/whistle/")
	allowed := false
	for _, f := range whistleFiles {
		if f == name {
			allowed = true
		}
	}
	path := filepath.Join(a.whistleDir(), filepath.FromSlash(name))
	if !allowed || !isFile(path) {
		http.Error(w, "speech model is not available yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if file, err := os.Open(path); err == nil {
		defer file.Close()
		if info, err := file.Stat(); err == nil {
			http.ServeContent(w, r, name, info.ModTime(), file)
			return
		}
	}
	http.Error(w, "could not read the speech model", http.StatusInternalServerError)
}
