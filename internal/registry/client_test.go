package registry

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testManifest = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:1111","size":2},"layers":[]}`

func TestFetchManifestBasicAuth(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="test-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", MediaTypeOCIManifest)
		_, _ = w.Write([]byte(testManifest))
	}))
	defer srv.Close()

	c := New(Options{
		Registry:    strings.TrimPrefix(srv.URL, "http://"),
		Repository:  "test/repo",
		Credentials: Credentials{Username: "admin", Password: "secret"},
		HTTPClient:  srv.Client(),
		PlainHTTP:   true,
	})
	body, mediaType, digest, err := c.FetchManifest(context.Background(), "latest")
	if err != nil {
		t.Fatalf("FetchManifest() error = %v", err)
	}
	if string(body) != testManifest {
		t.Fatalf("body = %q", body)
	}
	if mediaType != MediaTypeOCIManifest {
		t.Errorf("mediaType = %q", mediaType)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Errorf("digest = %q", digest)
	}
}

func TestFetchManifestBearerAuth(t *testing.T) {
	var tokenReached bool
	var tokenAuthOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenReached = true
			user, pass, ok := r.BasicAuth()
			tokenAuthOK = ok && user == "user" && pass == "pass"
			if r.URL.Query().Get("service") != "test-service" {
				t.Errorf("service = %q", r.URL.Query().Get("service"))
			}
			if r.URL.Query().Get("scope") != "repository:test/repo:pull" {
				t.Errorf("scope = %q", r.URL.Query().Get("scope"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "test-token",
				"expires_in": 300,
			})
		default:
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+srvURL(r)+`/token",service="test-service"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			_, _ = w.Write([]byte(testManifest))
		}
	}))
	defer srv.Close()

	c := New(Options{
		Registry:    strings.TrimPrefix(srv.URL, "http://"),
		Repository:  "test/repo",
		Credentials: Credentials{Username: "user", Password: "pass"},
		HTTPClient:  srv.Client(),
		PlainHTTP:   true,
	})
	if _, _, _, err := c.FetchManifest(context.Background(), "latest"); err != nil {
		t.Fatalf("FetchManifest() error = %v", err)
	}
	if !tokenReached {
		t.Fatal("未请求令牌服务")
	}
	if !tokenAuthOK {
		t.Fatal("令牌服务未收到正确的 Basic 认证")
	}
}

// srvURL 从请求中恢复测试服务器地址。
func srvURL(r *http.Request) string {
	return "http://" + r.Host
}

// TestBearerTokenRevokedRefreshesImmediately 验证缓存令牌被服务端吊销后，
// 首个 401 即强制刷新令牌，不会重复使用已失效的令牌多跑一趟。
func TestBearerTokenRevokedRefreshesImmediately(t *testing.T) {
	var mu sync.Mutex
	tokenCount := 0
	current := ""
	manifestHits := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			mu.Lock()
			tokenCount++
			current = fmt.Sprintf("token-%d", tokenCount)
			tok := current
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"token": tok, "expires_in": 300})
		default:
			mu.Lock()
			manifestHits++
			valid := r.Header.Get("Authorization") == "Bearer "+current
			mu.Unlock()
			if !valid {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+srvURL(r)+`/token",service="test-service"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", MediaTypeOCIManifest)
			_, _ = w.Write([]byte(testManifest))
		}
	}))
	defer srv.Close()

	c := New(Options{
		Registry:    strings.TrimPrefix(srv.URL, "http://"),
		Repository:  "test/repo",
		Credentials: Credentials{Username: "user", Password: "pass"},
		HTTPClient:  srv.Client(),
		PlainHTTP:   true,
	})

	// 第一次拉取成功，令牌 token-1 被缓存。
	if _, _, _, err := c.FetchManifest(context.Background(), "latest"); err != nil {
		t.Fatalf("FetchManifest() error = %v", err)
	}

	// 模拟服务端吊销 token-1。
	mu.Lock()
	current = "revoked"
	hitsBefore := manifestHits
	mu.Unlock()

	if _, _, _, err := c.FetchManifest(context.Background(), "latest"); err != nil {
		t.Fatalf("FetchManifest() error = %v", err)
	}
	mu.Lock()
	extraHits := manifestHits - hitsBefore
	tokens := tokenCount
	mu.Unlock()
	if extraHits != 2 {
		t.Fatalf("吊销后清单请求次数 = %d, want 2（一次被拒 + 一次换新令牌成功）", extraHits)
	}
	if tokens != 2 {
		t.Fatalf("令牌获取次数 = %d, want 2", tokens)
	}
}

func TestFetchBlobVerifiesDigest(t *testing.T) {
	payload := []byte("hello blob")
	digest := "sha256:" + sha256Hex(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := New(Options{
		Registry:   strings.TrimPrefix(srv.URL, "http://"),
		Repository: "test/repo",
		HTTPClient: srv.Client(),
		PlainHTTP:  true,
	})
	got, err := c.FetchBlob(context.Background(), digest, 1024)
	if err != nil {
		t.Fatalf("FetchBlob() error = %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("内容不一致")
	}
	if _, err := c.FetchBlob(context.Background(), "sha256:"+strings.Repeat("0", 64), 1024); err == nil {
		t.Fatal("摘要不匹配时应返回错误")
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
