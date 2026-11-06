package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestAppendCerts 验证证书文件的加载与跳过逻辑。
func TestAppendCerts(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	dir := t.TempDir()
	good := filepath.Join(dir, "good.pem")
	if err := os.WriteFile(good, pemData, 0o644); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	loaded := appendCerts(pool, []string{good, junk, filepath.Join(dir, "missing.pem")})
	if len(loaded) != 1 || loaded[0] != good {
		t.Fatalf("应只加载 %s，得到 %v", good, loaded)
	}
}

// TestRootCACandidates 验证候选路径包含 Termux 证书位置。
func TestRootCACandidates(t *testing.T) {
	t.Setenv("PREFIX", "/data/data/com.termux/files/usr")
	paths := rootCACandidates()
	want := []string{
		"/data/data/com.termux/files/usr/etc/tls/cert.pem",
		"/etc/tls/cert.pem",
	}
	for _, w := range want {
		found := false
		for _, p := range paths {
			if p == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("候选路径缺少 %s: %v", w, paths)
		}
	}
}

// TestLoadRootCAsRespectsEnv 验证用户指定 SSL_CERT_FILE 时不干预默认行为。
func TestLoadRootCAsRespectsEnv(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/tmp/ca.pem")
	if pool, note := loadRootCAs(); pool != nil || note != "" {
		t.Fatalf("应交由默认行为处理，得到 %v %q", pool, note)
	}
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "/tmp/certs")
	if pool, note := loadRootCAs(); pool != nil || note != "" {
		t.Fatalf("应交由默认行为处理，得到 %v %q", pool, note)
	}
}

// TestTLSWithLoadedRootCAs 端到端验证：自签证书服务器在默认客户端下失败，
// 通过 appendCerts 加载根证书后可以完成 TLS 握手。
func TestTLSWithLoadedRootCAs(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "cert.pem")
	if err := os.WriteFile(path, pemData, 0o644); err != nil {
		t.Fatal(err)
	}

	plain := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}}
	if _, err := plain.Get(srv.URL); err == nil {
		t.Fatal("默认信任库不包含自签证书，握手应当失败")
	}

	pool := x509.NewCertPool()
	if loaded := appendCerts(pool, []string{path}); len(loaded) != 1 {
		t.Fatalf("未加载到根证书: %v", loaded)
	}
	trusted := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
	}}
	resp, err := trusted.Get(srv.URL)
	if err != nil {
		t.Fatalf("加载根证书后仍失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码不符: %d", resp.StatusCode)
	}
}
