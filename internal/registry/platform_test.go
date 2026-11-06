package registry

import (
	"testing"
)

func TestNormalizeArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "amd64",
		"aarch64": "arm64",
		"arm64v8": "arm64",
		"armv7l":  "arm",
		"i386":    "386",
	}
	for input, want := range cases {
		if got := NormalizeArch(input); got != want {
			t.Errorf("NormalizeArch(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParsePlatform(t *testing.T) {
	p, err := ParsePlatform("linux/arm/v7")
	if err != nil {
		t.Fatal(err)
	}
	if p.OS != "linux" || p.Architecture != "arm" || p.Variant != "v7" {
		t.Fatalf("unexpected platform: %+v", p)
	}
	if p.String() != "linux/arm/v7" {
		t.Fatalf("String() = %q", p.String())
	}
	if _, err := ParsePlatform("amd64"); err == nil {
		t.Fatal("缺少 os 时应报错")
	}
}

func TestSelectManifest(t *testing.T) {
	manifests := []Descriptor{
		{Digest: "sha256:unknown", Platform: &Platform{OS: "unknown", Architecture: "unknown"}},
		{Digest: "sha256:amd64", Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		{Digest: "sha256:arm64", Platform: &Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
		{Digest: "sha256:armv7", Platform: &Platform{OS: "linux", Architecture: "arm", Variant: "v7"}},
	}

	d, ok := SelectManifest(manifests, Platform{OS: "linux", Architecture: "amd64"})
	if !ok || d.Digest != "sha256:amd64" {
		t.Fatalf("amd64 选择错误: %+v, %v", d, ok)
	}

	d, ok = SelectManifest(manifests, Platform{OS: "linux", Architecture: "arm64"})
	if !ok || d.Digest != "sha256:arm64" {
		t.Fatalf("arm64 应回退到 v8: %+v, %v", d, ok)
	}

	d, ok = SelectManifest(manifests, Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	if !ok || d.Digest != "sha256:armv7" {
		t.Fatalf("arm/v7 选择错误: %+v, %v", d, ok)
	}

	if _, ok := SelectManifest(manifests, Platform{OS: "linux", Architecture: "s390x"}); ok {
		t.Fatal("不存在的架构不应匹配")
	}

	platforms := Platforms(manifests)
	if len(platforms) != 3 {
		t.Fatalf("Platforms() = %v，应过滤 unknown", platforms)
	}
}

func TestParseChallenge(t *testing.T) {
	ch := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if ch == nil {
		t.Fatal("解析失败")
	}
	if ch.Scheme != "bearer" {
		t.Errorf("Scheme = %q", ch.Scheme)
	}
	if ch.Realm != "https://auth.docker.io/token" {
		t.Errorf("Realm = %q", ch.Realm)
	}
	if ch.Service != "registry.docker.io" {
		t.Errorf("Service = %q", ch.Service)
	}
	if ch.Scope != "repository:library/nginx:pull" {
		t.Errorf("Scope = %q", ch.Scope)
	}

	basic := parseChallenge(`Basic realm="registry"`)
	if basic == nil || basic.Scheme != "basic" || basic.Realm != "registry" {
		t.Fatalf("Basic 挑战解析错误: %+v", basic)
	}
}

func TestNormalizeRegistryKey(t *testing.T) {
	for _, key := range []string{
		"https://index.docker.io/v1/",
		"docker.io",
		"registry-1.docker.io",
		"index.docker.io",
	} {
		if got := normalizeRegistryKey(key); got != "docker.io" {
			t.Errorf("normalizeRegistryKey(%q) = %q", key, got)
		}
	}
}
