package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The app's dependency chain lives entirely under <dataDir>/runtime and is
// resolved at run time. Nothing is installed system-wide and no system PATH,
// PowerShell profile or user environment is consulted.
//
//	runtime/llama      llama.cpp builds, one folder per GPU backend
//	runtime/models     chat, embedding and draft models, and Whistle
//	runtime/audio      the speech runtime and its models

func (a *app) runtimeDir() string { return filepath.Join(a.dataDir, "runtime") }

// runtimeEnv is the environment for child programs (the model servers): the user's own
// PATH is deliberately not inherited, so DLLs from other installs (another CUDA toolkit, for
// example) cannot shadow the ones shipped next to each server. PSModulePath is dropped so a
// PowerShell 7 parent cannot break Windows PowerShell children.
func (a *app) runtimeEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.EqualFold(key, "PSModulePath") {
			continue
		}
		env = append(env, entry)
	}
	systemRoot := envValue(env, "SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return setEnvValue(env, "Path", strings.Join([]string{
		filepath.Join(systemRoot, "System32"),
		systemRoot,
		filepath.Join(systemRoot, "System32", "Wbem"),
	}, string(os.PathListSeparator)))
}

func (a *app) httpGet(url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(a.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("server returned %s", response.Status)
	}
	return response, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// retryFS retries a filesystem operation for up to ~30s. On Windows, antivirus
// or the indexer commonly holds handles on freshly extracted or just-executed
// binaries, which makes remove/rename fail transiently with "Access is denied".
func (a *app) retryFS(what string, op func() error) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s (is antivirus or another process holding files in %s?): %w", what, a.runtimeDir(), err)
}

func copyZipFile(file *zip.File, target string) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
