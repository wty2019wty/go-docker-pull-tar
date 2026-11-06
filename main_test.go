package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func buildLayer(t *testing.T) (blob []byte, diffID, digest string) {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("hello from fake registry")
	if err := tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	diffID = sha256Digest(tarBuf.Bytes())

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	blob = gzBuf.Bytes()
	digest = sha256Digest(blob)
	return
}

// TestRunEndToEndWithBasicAuth 用本地假仓库跑通认证、分片下载与 docker 打包。
func TestRunEndToEndWithBasicAuth(t *testing.T) {
	layerBlob, diffID, layerDigest := buildLayer(t)
	configBytes, _ := json.Marshal(map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
	})
	configDigest := sha256Digest(configBytes)

	manifestBytes, _ := json.Marshal(map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configBytes),
		},
		"layers": []map[string]interface{}{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
			"digest":    layerDigest,
			"size":      len(layerBlob),
		}},
	})

	blobs := map[string][]byte{
		configDigest: configBytes,
		layerDigest:  layerBlob,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "user" || pass != "pass" {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/manifests/latest") {
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			_, _ = w.Write(manifestBytes)
			return
		}
		idx := strings.LastIndex(r.URL.Path, "/blobs/")
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
		data, ok := blobs[r.URL.Path[idx+len("/blobs/"):]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			return
		}
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			_, _ = w.Write(data)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil ||
			end < 0 || end >= int64(len(data)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	dir := t.TempDir()
	output := filepath.Join(dir, "out.tar")

	opts := cliOptions{
		image:        host + "/test/repo:latest",
		username:     "user",
		password:     "pass",
		plainHTTP:    true,
		workers:      3,
		chunkSize:    256,
		minChunkSize: 256,
		retries:      2,
		format:       "docker",
		output:       output,
		cacheDir:     filepath.Join(dir, "cache"),
		quiet:        true,
	}
	if err := run(context.Background(), opts, ui.New(true, false)); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "manifest.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("生成的 tar 中缺少 manifest.json")
	}
}

// TestResolvePlatformVariant 验证 -platform 与 -os/-arch/-variant 组合时的变体处理。
func TestResolvePlatformVariant(t *testing.T) {
	cases := []struct {
		name     string
		opts     cliOptions
		wantOS   string
		wantArch string
		wantVar  string
	}{
		{"platform 变体在补充 -os 时保留", cliOptions{platform: "linux/arm/v6", osName: "linux"}, "linux", "arm", "v6"},
		{"platform 变体在架构相同时保留", cliOptions{platform: "linux/arm/v6", arch: "arm"}, "linux", "arm", "v6"},
		{"架构覆盖后重置变体", cliOptions{platform: "linux/arm/v7", arch: "arm64"}, "linux", "arm64", ""},
		{"显式 -variant 优先", cliOptions{platform: "linux/arm/v6", variant: "v7"}, "linux", "arm", "v7"},
		{"默认 arm 变体 v7", cliOptions{arch: "arm"}, "linux", "arm", "v7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, err := resolvePlatform(tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if p.OS != tc.wantOS || p.Architecture != tc.wantArch || p.Variant != tc.wantVar {
				t.Fatalf("got %s, want %s/%s/%s", p.String(), tc.wantOS, tc.wantArch, tc.wantVar)
			}
		})
	}
}
