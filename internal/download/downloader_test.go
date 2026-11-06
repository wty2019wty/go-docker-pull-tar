package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wty2019wty/go-docker-pull-tar/internal/registry"
	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

type rangeRecorder struct {
	mu     sync.Mutex
	counts map[string]int
}

func (r *rangeRecorder) add(header string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[header]++
}

func (r *rangeRecorder) get(header string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[header]
}

func (r *rangeRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.counts {
		n += c
	}
	return n
}

// newTestRegistry 启动一个支持 Range 的极简 Blob 服务。
func newTestRegistry(t *testing.T, payload []byte) (*httptest.Server, *rangeRecorder) {
	t.Helper()
	rec := &rangeRecorder{counts: make(map[string]int)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}
		rec.add(rangeHeader)
		var start, end int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil ||
			start < 0 || end < start || end >= int64(len(payload)) {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestDownloadChunkedAndResume(t *testing.T) {
	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i * 31 % 251)
	}
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	srv, rec := newTestRegistry(t, payload)
	host := strings.TrimPrefix(srv.URL, "http://")
	client := registry.New(registry.Options{
		Registry:   host,
		Repository: "test/repo",
		HTTPClient: srv.Client(),
		PlainHTTP:  true,
		UserAgent:  "test",
	})

	cacheDir := t.TempDir()
	u := ui.New(true, false)
	progress := ui.NewProgress(u)
	progress.Start()
	defer progress.Stop()

	d, err := New(Options{
		Client:         client,
		HTTPClient:     srv.Client(),
		CacheDir:       cacheDir,
		ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Platform:       "linux/amd64",
		Workers:        4,
		ChunkSize:      1024,
		MinChunkSize:   1024,
		Retries:        2,
		Progress:       progress,
		UI:             u,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 预置第一个分片，模拟断点续传。
	h := strings.TrimPrefix(digest, "sha256:")
	partsDir := filepath.Join(cacheDir, "parts", h)
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partsDir, "0-1023"), payload[:1024], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.Download(context.Background(), []Blob{{Digest: digest, Size: int64(len(payload))}}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	obj, err := ObjectPath(cacheDir, digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("下载内容不一致")
	}

	if rec.total() < 4 {
		t.Fatalf("Range 请求次数 = %d，分片下载可能未生效", rec.total())
	}
	if n := rec.get("bytes=0-1023"); n != 0 {
		t.Fatalf("已存在的分片被重复请求 %d 次", n)
	}
	if n := rec.get("bytes=0-0"); n != 1 {
		t.Fatalf("Range 探测请求次数 = %d，期望 1", n)
	}
}

func TestDownloadSingleStream(t *testing.T) {
	payload := make([]byte, 2048)
	for i := range payload {
		payload[i] = byte(i)
	}
	sum := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	srv, rec := newTestRegistry(t, payload)
	host := strings.TrimPrefix(srv.URL, "http://")
	client := registry.New(registry.Options{
		Registry:   host,
		Repository: "test/repo",
		HTTPClient: srv.Client(),
		PlainHTTP:  true,
		UserAgent:  "test",
	})

	cacheDir := t.TempDir()
	u := ui.New(true, false)
	progress := ui.NewProgress(u)
	d, err := New(Options{
		Client:         client,
		HTTPClient:     srv.Client(),
		CacheDir:       cacheDir,
		ManifestDigest: "sha256:" + strings.Repeat("b", 64),
		Platform:       "linux/amd64",
		Workers:        2,
		ChunkSize:      0,
		MinChunkSize:   0,
		Retries:        1,
		Progress:       progress,
		UI:             u,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Download(context.Background(), []Blob{{Digest: digest, Size: int64(len(payload))}}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if rec.total() != 0 {
		t.Fatalf("单流下载不应发送 Range 请求，实际 %d 次", rec.total())
	}
	obj, _ := ObjectPath(cacheDir, digest)
	got, _ := os.ReadFile(obj)
	if string(got) != string(payload) {
		t.Fatal("单流下载内容不一致")
	}
}
