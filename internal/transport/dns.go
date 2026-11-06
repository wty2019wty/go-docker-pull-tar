package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// fallbackDNSServers 是系统 DNS 配置不可用时（例如 Android Termux 缺少
// /etc/resolv.conf）使用的公共 DNS，优先国内公共 DNS，避免境外 DNS 被劫持。
var fallbackDNSServers = []string{
	"223.5.5.5:53",    // 阿里 AliDNS
	"119.29.29.29:53", // 腾讯 DNSPod
	"114.114.114.114:53",
	"8.8.8.8:53", // Google
}

// 以下变量可在测试中替换。
var (
	resolvConfPath = "/etc/resolv.conf"
	runtimeGOOS    = runtime.GOOS
)

// selectDNSServers 决定使用的 DNS 服务器列表，返回 nil 表示使用系统解析器。
//
// 优先级：
//  1. 显式参数（-dns）；
//  2. GDOCKER_DNS 环境变量；
//  3. 系统 /etc/resolv.conf 可用（仅 Linux/Android 检查）时使用系统解析器；
//  4. 否则回退公共 DNS——纯 Go 二进制在 Termux 等环境读不到 resolv.conf 时，
//     默认会向 127.0.0.1:53 / [::1]:53 查询并报 connection refused。
func selectDNSServers(explicit string) (servers []string, source string, err error) {
	s := strings.TrimSpace(explicit)
	if s == "" {
		s = strings.TrimSpace(os.Getenv("GDOCKER_DNS"))
	}
	if s != "" {
		servers, err := parseDNSServers(s)
		if err != nil {
			return nil, "", err
		}
		return servers, "使用自定义 DNS 服务器", nil
	}

	if runtimeGOOS != "linux" && runtimeGOOS != "android" {
		// Windows/macOS 等平台由操作系统提供解析能力，无需干预。
		return nil, "", nil
	}
	if ns := loadResolvNameServers(resolvConfPath); len(ns) > 0 {
		return nil, "", nil
	}
	return fallbackDNSServers, "未检测到可用的系统 DNS 配置（" + resolvConfPath + "），回退公共 DNS", nil
}

// parseDNSServers 解析逗号/分号/空白分隔的 DNS 服务器列表。
func parseDNSServers(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		addr, err := normalizeDNSServer(f)
		if err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	if len(out) == 0 {
		return nil, errors.New("未指定 DNS 服务器")
	}
	return out, nil
}

// normalizeDNSServer 把 "IP"、"IP:端口"、"[IPv6]:端口" 统一为 host:port 形式，
// 默认端口 53。仅接受 IP 地址（用域名指定 DNS 服务器会陷入鸡生蛋困境）。
func normalizeDNSServer(s string) (string, error) {
	if host, port, err := net.SplitHostPort(s); err == nil {
		if net.ParseIP(host) == nil {
			return "", fmt.Errorf("无效的 DNS 服务器 %q（请使用 IP 地址）", s)
		}
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			return "", fmt.Errorf("无效的 DNS 端口 %q", port)
		}
		return net.JoinHostPort(host, port), nil
	}
	if net.ParseIP(s) != nil {
		return net.JoinHostPort(s, "53"), nil
	}
	return "", fmt.Errorf("无效的 DNS 服务器 %q（请使用 IP 或 IP:端口）", s)
}

// loadResolvNameServers 从 resolv.conf 风格的文件中提取 nameserver 列表，
// 文件不存在或没有 nameserver 时返回 nil。
func loadResolvNameServers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if addr, err := normalizeDNSServer(fields[1]); err == nil {
			out = append(out, addr)
		}
	}
	return out
}

// newDNSResolver 创建指向指定 DNS 服务器的解析器。查询在服务器之间轮询，
// 单个服务器不可达时自动尝试其余服务器；UDP 查询被截断时 Go 解析器会自动
// 改用 TCP 重新查询，Dial 透传 network 即可。
func newDNSResolver(servers []string) *net.Resolver {
	d := &net.Dialer{Timeout: 10 * time.Second}
	var counter atomic.Uint32

	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// address 是解析器配置中的默认地址（如 127.0.0.1:53），忽略之，
			// 改为在自定义服务器列表中轮询。
			start := int(counter.Add(1)-1) % len(servers)
			var lastErr error
			for i := 0; i < len(servers); i++ {
				server := servers[(start+i)%len(servers)]
				conn, err := d.DialContext(ctx, network, server)
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, fmt.Errorf("所有 DNS 服务器均不可用: %w", lastErr)
		},
	}
}
