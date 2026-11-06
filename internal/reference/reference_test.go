package reference

import (
	"strings"
	"testing"
)

const sampleDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParse(t *testing.T) {
	tests := []struct {
		input      string
		registry   string
		repository string
		tag        string
		digest     string
		name       string
	}{
		{"nginx", DockerHubRegistry, "library/nginx", "latest", "", "nginx:latest"},
		{"nginx:1.25", DockerHubRegistry, "library/nginx", "1.25", "", "nginx:1.25"},
		{"library/nginx", DockerHubRegistry, "library/nginx", "latest", "", "nginx:latest"},
		{"docker.io/library/nginx:latest", DockerHubRegistry, "library/nginx", "latest", "", "nginx:latest"},
		{"user/app:v1", DockerHubRegistry, "user/app", "v1", "", "user/app:v1"},
		{"ghcr.io/owner/repo:tag", "ghcr.io", "owner/repo", "tag", "", "ghcr.io/owner/repo:tag"},
		{"localhost:5000/team/app", "localhost:5000", "team/app", "latest", "", "localhost:5000/team/app:latest"},
		{"nginx@" + sampleDigest, DockerHubRegistry, "library/nginx", "", sampleDigest, "nginx:latest"},
		{"docker://alpine:3.20", DockerHubRegistry, "library/alpine", "3.20", "", "alpine:3.20"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref, err := Parse(tt.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if ref.Registry != tt.registry {
				t.Errorf("Registry = %q, want %q", ref.Registry, tt.registry)
			}
			if ref.Repository != tt.repository {
				t.Errorf("Repository = %q, want %q", ref.Repository, tt.repository)
			}
			if ref.Tag != tt.tag {
				t.Errorf("Tag = %q, want %q", ref.Tag, tt.tag)
			}
			if ref.Digest != tt.digest {
				t.Errorf("Digest = %q, want %q", ref.Digest, tt.digest)
			}
			if got := ref.Name(); got != tt.name {
				t.Errorf("Name() = %q, want %q", got, tt.name)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	for _, input := range []string{
		"",
		"UPPER/repo",
		"nginx@sha256:1234",
		"nginx@sha512:" + strings.Repeat("a", 64),
		"nginx:",
	} {
		if _, err := Parse(input); err == nil {
			t.Errorf("Parse(%q) 应该返回错误", input)
		}
	}
}

func TestUseRegistryKeepsName(t *testing.T) {
	ref, err := Parse("nginx:latest")
	if err != nil {
		t.Fatal(err)
	}
	ref.UseRegistry("docker.1ms.run")
	if ref.Registry != "docker.1ms.run" {
		t.Fatalf("Registry = %q", ref.Registry)
	}
	if ref.Name() != "nginx:latest" {
		t.Fatalf("Name() = %q，镜像站不应影响生成的名字", ref.Name())
	}
}

func TestUseRegistryStripsScheme(t *testing.T) {
	ref, _ := Parse("nginx")
	ref.UseRegistry("http://mirror.local:5000/")
	if ref.Registry != "mirror.local:5000" {
		t.Fatalf("Registry = %q", ref.Registry)
	}
}
