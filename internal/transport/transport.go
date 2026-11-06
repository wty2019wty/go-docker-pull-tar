// Package transport 构建带代理、TLS 配置的 HTTP 客户端。
package transport

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Options 是构建 HTTP 客户端的参数。
type Options struct {
	// Proxy 支持 http、https、socks5、socks5h，留空则读取环境变量。
	Proxy string
	// Insecure 跳过 TLS 证书校验。
	Insecure bool
	// MaxConnsPerHost 控制单主机最大连接数。
	MaxConnsPerHost int
	// DNS 指定自定义 DNS 服务器（逗号分隔，如 223.5.5.5,8.8.8.8），留空则自动检测。
	// 在缺少 /etc/resolv.conf 的环境（如 Android Termux）会自动回退公共 DNS。
	DNS string
	// Log 输出重要提示日志。
	Log func(format string, args ...interface{})
	// Verbose 输出调试日志。
	Verbose func(format string, args ...interface{})
}

// New 创建 HTTP 客户端。
func New(opts Options) (*http.Client, error) {
	if opts.MaxConnsPerHost <= 0 {
		opts.MaxConnsPerHost = 64
	}
	if opts.Verbose == nil {
		opts.Verbose = func(string, ...interface{}) {}
	}
	if opts.Log == nil {
		opts.Log = func(string, ...interface{}) {}
	}

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	dnsServers, dnsSource, err := selectDNSServers(opts.DNS)
	if err != nil {
		return nil, err
	}
	if dnsServers != nil {
		dialer.Resolver = newDNSResolver(dnsServers)
		opts.Log("%s: %s", dnsSource, strings.Join(dnsServers, ", "))
	}

	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   opts.MaxConnsPerHost,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		DisableCompression:    true,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: opts.Insecure, //nolint:gosec // 由用户显式指定
		},
	}

	if strings.TrimSpace(opts.Proxy) != "" {
		proxyURL, err := url.Parse(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("无效的代理地址 %q: %w", opts.Proxy, err)
		}
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https":
			tr.Proxy = http.ProxyURL(proxyURL)
			opts.Verbose("使用 HTTP 代理: %s", proxyURL.Redacted())
		case "socks5", "socks5h", "socks":
			host := proxyURL.Host
			if host == "" {
				host = proxyURL.Path
			}
			if !strings.Contains(host, ":") {
				host += ":1080"
			}
			var auth *socksAuth
			if proxyURL.User != nil {
				pass, _ := proxyURL.User.Password()
				auth = &socksAuth{User: proxyURL.User.Username(), Pass: pass}
			}
			sd := &socks5Dialer{proxyAddr: host, auth: auth, dialer: dialer}
			tr.Proxy = nil
			tr.DialContext = sd.DialContext
			opts.Verbose("使用 SOCKS5 代理: %s", host)
		case "none", "direct":
			tr.Proxy = nil
			opts.Verbose("不使用代理")
		default:
			return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5）", proxyURL.Scheme)
		}
	} else if hasProxyEnv() {
		opts.Verbose("检测到系统代理环境变量，将自动使用")
	}

	return &http.Client{
		Transport: tr,
		// 不设置整体超时，大文件下载可能持续很久。
		Timeout: 0,
	}, nil
}

func hasProxyEnv() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if strings.TrimSpace(os.Getenv(k)) != "" {
			return true
		}
	}
	return false
}

type socksAuth struct {
	User string
	Pass string
}

// socks5Dialer 实现最小可用的 SOCKS5 客户端（支持无认证与用户名密码认证）。
type socks5Dialer struct {
	proxyAddr string
	auth      *socksAuth
	dialer    *net.Dialer
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := d.dialer.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接 SOCKS5 代理失败: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	if err = d.handshake(conn, addr); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn, target string) error {
	methods := []byte{0x00}
	if d.auth != nil {
		methods = []byte{0x00, 0x02}
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("SOCKS5 握手失败: %w", err)
	}

	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("SOCKS5 握手失败: %w", err)
	}
	if buf[0] != 0x05 {
		return errors.New("SOCKS5 代理返回了无效版本")
	}
	switch buf[1] {
	case 0x00:
		// 无需认证
	case 0x02:
		if d.auth == nil {
			return errors.New("SOCKS5 代理要求用户名密码认证")
		}
		if err := d.authenticate(conn); err != nil {
			return err
		}
	case 0xFF:
		return errors.New("SOCKS5 代理不接受任何认证方式")
	default:
		return fmt.Errorf("SOCKS5 代理要求不支持的认证方式: 0x%02x", buf[1])
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("无效的目标地址 %q: %w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("无效的目标端口: %q", portStr)
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("目标域名过长")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))

	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 连接请求失败: %w", err)
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("读取 SOCKS5 响应失败: %w", err)
	}
	if header[0] != 0x05 {
		return errors.New("SOCKS5 响应版本无效")
	}
	if header[1] != 0x00 {
		return fmt.Errorf("SOCKS5 代理拒绝连接: %s", socksReplyText(header[1]))
	}

	var skip int
	switch header[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return err
		}
		skip = int(length[0])
	default:
		return fmt.Errorf("SOCKS5 响应地址类型无效: 0x%02x", header[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip+2)); err != nil {
		return err
	}
	return nil
}

func (d *socks5Dialer) authenticate(conn net.Conn) error {
	user := []byte(d.auth.User)
	pass := []byte(d.auth.Pass)
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("SOCKS5 用户名或密码过长")
	}
	req := []byte{0x01, byte(len(user))}
	req = append(req, user...)
	req = append(req, byte(len(pass)))
	req = append(req, pass...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("SOCKS5 认证请求失败: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("读取 SOCKS5 认证响应失败: %w", err)
	}
	if resp[0] != 0x01 || resp[1] != 0x00 {
		return errors.New("SOCKS5 用户名或密码错误")
	}
	return nil
}

func socksReplyText(code byte) string {
	switch code {
	case 1:
		return "服务器内部错误"
	case 2:
		return "连接不被规则允许"
	case 3:
		return "网络不可达"
	case 4:
		return "主机不可达"
	case 5:
		return "连接被拒绝"
	case 6:
		return "TTL 过期"
	case 7:
		return "不支持的命令"
	case 8:
		return "不支持的地址类型"
	default:
		return fmt.Sprintf("错误码 %d", code)
	}
}
