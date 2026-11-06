package transport

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseDNSServers(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "单个 IP", input: "223.5.5.5", want: []string{"223.5.5.5:53"}},
		{name: "多个 IP 逗号分隔", input: "223.5.5.5,8.8.8.8", want: []string{"223.5.5.5:53", "8.8.8.8:53"}},
		{name: "空白与分号混合", input: " 223.5.5.5 ; 8.8.8.8\t1.1.1.1 ", want: []string{"223.5.5.5:53", "8.8.8.8:53", "1.1.1.1:53"}},
		{name: "带端口", input: "127.0.0.1:5353", want: []string{"127.0.0.1:5353"}},
		{name: "裸 IPv6", input: "2001:4860:4860::8888", want: []string{"[2001:4860:4860::8888]:53"}},
		{name: "带端口 IPv6", input: "[2001:4860:4860::8888]:5353", want: []string{"[2001:4860:4860::8888]:5353"}},
		{name: "域名被拒绝", input: "dns.example.com", wantErr: true},
		{name: "非法 IP 被拒绝", input: "999.1.1.1", wantErr: true},
		{name: "非法端口被拒绝", input: "1.1.1.1:70000", wantErr: true},
		{name: "空输入", input: " , ;", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDNSServers(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("得到 %v，期望 %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, tt.want)
				}
			}
		})
	}
}

func TestLoadResolvNameServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	content := `# 注释行
; 另一种注释
nameserver 223.5.5.5
search example.com
nameserver 8.8.8.8:5353
nameserver not-an-ip
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadResolvNameServers(path)
	want := []string{"223.5.5.5:53", "8.8.8.8:5353"}
	if len(got) != len(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("得到 %v，期望 %v", got, want)
		}
	}

	if got := loadResolvNameServers(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("文件不存在时应返回 nil，得到 %v", got)
	}
}

func TestSelectDNSServers(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-file")
	usable := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(usable, []byte("nameserver 223.5.5.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath, origGOOS := resolvConfPath, runtimeGOOS
	t.Cleanup(func() {
		resolvConfPath, runtimeGOOS = origPath, origGOOS
	})
	t.Setenv("GDOCKER_DNS", "")

	// 显式参数优先于一切。
	resolvConfPath, runtimeGOOS = usable, "linux"
	servers, source, err := selectDNSServers("1.1.1.1")
	if err != nil || len(servers) != 1 || servers[0] != "1.1.1.1:53" || source == "" {
		t.Fatalf("显式参数失败: %v %v %q", servers, source, err)
	}

	// 环境变量次之。
	t.Setenv("GDOCKER_DNS", "8.8.4.4")
	servers, _, err = selectDNSServers("")
	if err != nil || len(servers) != 1 || servers[0] != "8.8.4.4:53" {
		t.Fatalf("环境变量失败: %v %v", servers, err)
	}
	t.Setenv("GDOCKER_DNS", "")

	// Linux 上 resolv.conf 可用 → 系统解析器。
	resolvConfPath, runtimeGOOS = usable, "linux"
	servers, _, err = selectDNSServers("")
	if err != nil || servers != nil {
		t.Fatalf("应使用系统解析器，得到 %v（%v）", servers, err)
	}

	// Android Termux 场景：resolv.conf 缺失 → 回退公共 DNS。
	resolvConfPath, runtimeGOOS = missing, "android"
	servers, source, err = selectDNSServers("")
	if err != nil || len(servers) == 0 {
		t.Fatalf("应回退公共 DNS，得到 %v（%v）", servers, err)
	}
	if source == "" {
		t.Fatal("回退时应给出提示来源")
	}

	// Windows 等平台不受影响。
	resolvConfPath, runtimeGOOS = missing, "windows"
	servers, _, err = selectDNSServers("")
	if err != nil || servers != nil {
		t.Fatalf("Windows 应使用系统解析器，得到 %v（%v）", servers, err)
	}
}

// startFakeDNS 启动一个极简 DNS 服务器，对 A 查询返回固定 IP，
// 对其他类型返回无记录的成功响应。
func startFakeDNS(t *testing.T, ip net.IP) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, raddr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := buildDNSResponse(buf[:n], ip); resp != nil {
				_, _ = pc.WriteTo(resp, raddr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

// buildDNSResponse 按查询报文构造应答：回显 ID 与问题段，
// qtype 为 A 时附带一条 A 记录。
func buildDNSResponse(query []byte, ip net.IP) []byte {
	if len(query) < 12 {
		return nil
	}
	// 定位问题段结尾（QDCOUNT 固定为 1）。
	q := 12
	for q < len(query) && query[q] != 0 {
		q += int(query[q]) + 1
	}
	q++    // 根标签
	q += 4 // qtype + qclass
	if q > len(query) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(query[q-4 : q-2])

	var resp []byte
	resp = append(resp, query[:2]...) // ID
	resp = append(resp, 0x81, 0x80)   // QR | RD | RA
	resp = append(resp, 0x00, 0x01)   // QDCOUNT
	if qtype == 1 && ip.To4() != nil {
		resp = append(resp, 0x00, 0x01) // ANCOUNT
	} else {
		resp = append(resp, 0x00, 0x00)
	}
	resp = append(resp, 0x00, 0x00, 0x00, 0x00) // NSCOUNT | ARCOUNT
	resp = append(resp, query[12:q]...)         // 问题段原样回显
	if qtype == 1 && ip.To4() != nil {
		resp = append(resp, 0xc0, 0x0c)             // 名称指针
		resp = append(resp, 0x00, 0x01)             // type A
		resp = append(resp, 0x00, 0x01)             // class IN
		resp = append(resp, 0x00, 0x00, 0x00, 0x3c) // TTL 60
		resp = append(resp, 0x00, 0x04)             // rdlength
		resp = append(resp, ip.To4()...)
	}
	return resp
}

func TestDNSResolverLookup(t *testing.T) {
	addr := startFakeDNS(t, net.IPv4(1, 2, 3, 4))
	r := newDNSResolver([]string{addr})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := r.LookupIPAddr(ctx, "test.example")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(ips) == 0 || !ips[0].IP.Equal(net.IPv4(1, 2, 3, 4)) {
		t.Fatalf("解析结果不符: %v", ips)
	}
}

func TestDNSResolverDialFallback(t *testing.T) {
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
			_ = conn.Close()
		}
	}()

	// 第一台服务器（127.0.0.1:1）TCP 拒绝连接，应自动回退到第二台。
	// 传入 Dial 的 address 是解析器默认地址，必须被忽略。
	dead := "127.0.0.1:1"
	r := newDNSResolver([]string{dead, ln.Addr().String()})
	for i := 0; i < 3; i++ {
		conn, err := r.Dial(context.Background(), "tcp", "192.0.2.1:53")
		if err != nil {
			t.Fatalf("第 %d 次 Dial 未回退成功: %v", i+1, err)
		}
		_ = conn.Close()
	}
}
