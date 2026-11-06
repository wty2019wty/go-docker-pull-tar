package transport

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// startSocks5Proxy 启动一个仅支持无认证 CONNECT 的极简 SOCKS5 代理。
func startSocks5Proxy(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks5(conn)
		}
	}()
	return ln.Addr().String()
}

func handleSocks5(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)

	header := make([]byte, 2)
	if _, err := io.ReadFull(br, header); err != nil {
		return
	}
	if header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	if req[0] != 5 || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(br, l); err != nil {
			return
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(br, name); err != nil {
			return
		}
		host = string(name)
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(br, ip); err != nil {
			return
		}
		host = net.IP(ip).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)

	upstream, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, br)
		if c, ok := upstream.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	<-done
}

func TestSOCKS5Proxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("via-socks5"))
	}))
	defer target.Close()

	proxyAddr := startSocks5Proxy(t)
	client, err := New(Options{Proxy: "socks5://" + proxyAddr})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("通过 SOCKS5 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-socks5" {
		t.Fatalf("响应 = %q", body)
	}
}

func TestInvalidProxy(t *testing.T) {
	if _, err := New(Options{Proxy: "ftp://example.com:21"}); err == nil {
		t.Fatal("不支持的代理协议应报错")
	}
}
