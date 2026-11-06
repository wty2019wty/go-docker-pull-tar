package transport

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
)

// loadRootCAs 构建 TLS 根证书池，补全 Go 找不到系统 CA 的环境。
//
// 纯 Go 二进制只从固定路径读取根证书（见 crypto/x509），Android Termux 的
// ca-certificates 包安装在 $PREFIX/etc/tls/cert.pem，不在默认搜索路径里，
// 会导致所有 HTTPS 请求报 x509: certificate signed by unknown authority。
//
// 返回 nil 表示无需干预，沿用 crypto/x509 的默认行为（包括
// SSL_CERT_FILE / SSL_CERT_DIR 环境变量）。第二个返回值是给用户的提示。
func loadRootCAs() (*x509.CertPool, string) {
	if os.Getenv("SSL_CERT_FILE") != "" || os.Getenv("SSL_CERT_DIR") != "" {
		// 用户已显式指定证书文件/目录，交由 crypto/x509 默认处理。
		return nil, ""
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	loaded := appendCerts(pool, rootCACandidates())
	if len(loaded) == 0 {
		return nil, ""
	}
	return pool, "已加载额外的 CA 根证书: " + strings.Join(loaded, ", ")
}

// rootCACandidates 返回 Go 默认搜索路径之外的 CA 根证书候选位置。
func rootCACandidates() []string {
	var paths []string
	// Termux：$PREFIX/etc/tls/cert.pem（PREFIX 由 Termux 自动导出）。
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		paths = append(paths,
			filepath.Join(prefix, "etc", "tls", "cert.pem"),
			filepath.Join(prefix, "etc", "ssl", "cert.pem"),
		)
	}
	// termux-chroot（proot）环境下 $PREFIX 对应 /，以及 Termux 默认安装位置。
	paths = append(paths,
		"/etc/tls/cert.pem",
		"/data/data/com.termux/files/usr/etc/tls/cert.pem",
	)
	return paths
}

// appendCerts 把候选文件中的 PEM 证书加入池，返回成功加载的文件列表。
func appendCerts(pool *x509.CertPool, paths []string) []string {
	var loaded []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if pool.AppendCertsFromPEM(data) {
			loaded = append(loaded, p)
		}
	}
	return loaded
}
