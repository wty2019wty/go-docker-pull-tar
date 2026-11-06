package registry

import (
	"fmt"
	"runtime"
	"strings"
)

// NormalizeArch 将常见架构别名转换为 OCI 架构名。
func NormalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "", "auto":
		return runtime.GOARCH
	case "x86_64", "x64", "amd64":
		return "amd64"
	case "aarch64", "arm64", "arm64v8", "arm64/v8":
		return "arm64"
	case "armv7", "armv7l", "armhf", "arm/v7", "arm":
		return "arm"
	case "armv6", "armv6l", "arm/v6":
		return "arm"
	case "i386", "i686", "386", "x86":
		return "386"
	case "ppc64le":
		return "ppc64le"
	case "s390x":
		return "s390x"
	case "riscv64":
		return "riscv64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// NormalizeVariant 规范化变体名。
func NormalizeVariant(arch, variant string) string {
	v := strings.ToLower(strings.TrimSpace(variant))
	arch = NormalizeArch(arch)
	if v == "" {
		if arch == "arm" {
			// Docker Hub 上 arm 通常使用 v7
			return "v7"
		}
		return ""
	}
	if arch == "arm" && (v == "armv6" || v == "armv6l") {
		return "v6"
	}
	if arch == "arm" && (v == "armv7" || v == "armv7l" || v == "armhf") {
		return "v7"
	}
	if !strings.HasPrefix(v, "v") && arch == "arm" {
		return "v" + v
	}
	return v
}

// ParsePlatform 解析 linux/amd64、linux/arm/v7 形式的平台。
func ParsePlatform(s string) (Platform, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultPlatform(), nil
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return Platform{}, fmt.Errorf("平台格式应为 os/arch[/variant]，例如 linux/amd64: %q", s)
	}
	p := Platform{
		OS:           strings.ToLower(parts[0]),
		Architecture: NormalizeArch(parts[1]),
	}
	if len(parts) >= 3 {
		p.Variant = NormalizeVariant(p.Architecture, parts[2])
	}
	if p.OS == "" || p.Architecture == "" {
		return Platform{}, fmt.Errorf("无效的平台: %q", s)
	}
	return p, nil
}

// DefaultPlatform 返回默认平台：始终为 Linux，架构取当前主机架构。
// Docker 镜像绝大多数是 Linux 镜像，因此在 Windows/macOS 上也默认 linux。
func DefaultPlatform() Platform {
	return Platform{
		OS:           "linux",
		Architecture: NormalizeArch(runtime.GOARCH),
	}
}

// Match 判断候选平台是否匹配目标平台。
// 目标未指定 variant 时，优先匹配无 variant 的条目。
func Match(want Platform, candidate Platform) bool {
	if !strings.EqualFold(want.OS, candidate.OS) {
		return false
	}
	if NormalizeArch(want.Architecture) != NormalizeArch(candidate.Architecture) {
		return false
	}
	if want.Variant != "" {
		return NormalizeVariant(want.Architecture, want.Variant) ==
			NormalizeVariant(candidate.Architecture, candidate.Variant)
	}
	return true
}

// SelectManifest 从索引中选择目标平台的清单。
func SelectManifest(manifests []Descriptor, want Platform) (Descriptor, bool) {
	var fallback *Descriptor
	for i := range manifests {
		d := manifests[i]
		if d.Platform == nil {
			continue
		}
		if !Match(want, *d.Platform) {
			continue
		}
		if want.Variant == "" && d.Platform.Variant != "" {
			if fallback == nil {
				fallback = &d
			}
			continue
		}
		return d, true
	}
	if fallback != nil {
		return *fallback, true
	}
	return Descriptor{}, false
}

// Platforms 返回索引中可用的真实平台列表（去重，过滤 attestation 条目）。
func Platforms(manifests []Descriptor) []string {
	seen := make(map[string]bool)
	var out []string
	for _, d := range manifests {
		if d.Platform == nil || !d.Platform.IsReal() {
			continue
		}
		s := d.Platform.String()
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// IsReal 判断平台是否为真实可运行平台（排除 BuildKit attestation 的 unknown/unknown）。
func (p Platform) IsReal() bool {
	return p.OS != "" && p.OS != "unknown" && p.Architecture != "" && p.Architecture != "unknown"
}
