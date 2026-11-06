// Package reference 负责解析和规范化 Docker 镜像引用。
//
// 支持的形式：
//
//	nginx
//	nginx:1.25
//	library/nginx:latest
//	docker.io/library/nginx:latest
//	ghcr.io/owner/repo:tag
//	registry.example.com:5000/team/app@sha256:...
package reference

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// DockerHub 是 Docker 官方仓库的域名。
	DockerHub = "docker.io"
	// DockerHubRegistry 是 Docker Hub 实际使用的 Registry API 地址。
	DockerHubRegistry = "registry-1.docker.io"
)

var (
	repoComponentRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)
	tagRe           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe        = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[0-9a-fA-F]{32,}$`)
)

// Reference 描述一个已经解析完成的镜像引用。
type Reference struct {
	// Registry 是实际发起请求的 Registry 地址（使用镜像站时会被替换）。
	Registry string
	// OriginalRegistry 是用户原始输入中的 Registry，用于生成镜像名称。
	OriginalRegistry string
	// Repository 是仓库路径，例如 library/nginx。
	Repository string
	// Tag 是标签，可能为空（例如使用摘要拉取时）。
	Tag string
	// Digest 是摘要，例如 sha256:xxxx。
	Digest string
}

// Parse 解析镜像引用。
func Parse(input string) (*Reference, error) {
	s := strings.TrimSpace(input)
	s = strings.TrimPrefix(s, "docker://")
	if s == "" {
		return nil, errors.New("镜像名称不能为空")
	}

	if strings.ContainsAny(s, " \t\r\n") {
		return nil, fmt.Errorf("镜像名称中不能包含空白字符: %q", input)
	}

	var digest string
	if i := strings.IndexByte(s, '@'); i >= 0 {
		digest = s[i+1:]
		s = s[:i]
		if !digestRe.MatchString(digest) {
			return nil, fmt.Errorf("无效的镜像摘要: %q", digest)
		}
		if !strings.HasPrefix(digest, "sha256:") {
			return nil, fmt.Errorf("暂不支持该摘要算法（仅支持 sha256）: %q", digest)
		}
	}

	var tag string
	if i := strings.LastIndexByte(s, ':'); i > strings.LastIndexByte(s, '/') {
		tag = s[i+1:]
		s = s[:i]
		if !tagRe.MatchString(tag) {
			return nil, fmt.Errorf("无效的镜像标签: %q", tag)
		}
	}

	host := ""
	repo := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		first := s[:i]
		// 第一段包含 '.'、':' 或等于 localhost 时，视为仓库地址。
		if strings.ContainsAny(first, ".:") || first == "localhost" || strings.HasPrefix(first, "[") {
			host = first
			repo = s[i+1:]
		}
	}

	if repo == "" {
		return nil, fmt.Errorf("无效的镜像名称: %q", input)
	}

	original := host
	registry := host
	if host == "" || host == DockerHub || host == "index.docker.io" || host == DockerHubRegistry {
		original = DockerHub
		registry = DockerHubRegistry
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}

	if err := validateRepository(repo); err != nil {
		return nil, err
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}

	return &Reference{
		Registry:         registry,
		OriginalRegistry: original,
		Repository:       repo,
		Tag:              tag,
		Digest:           digest,
	}, nil
}

func validateRepository(repo string) error {
	for _, part := range strings.Split(repo, "/") {
		if !repoComponentRe.MatchString(part) {
			return fmt.Errorf("无效的仓库路径 %q（仓库名必须为小写）", repo)
		}
	}
	return nil
}

// UseRegistry 将请求地址切换到镜像站，但保留原始仓库名用于打标签。
func (r *Reference) UseRegistry(host string) {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	if host == "" {
		return
	}
	r.Registry = host
}

// Reference 返回请求清单时使用的引用（摘要优先）。
func (r *Reference) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Name 返回用于 docker load 的完整镜像名，例如 nginx:latest。
func (r *Reference) Name() string {
	repo := r.Repository
	if r.OriginalRegistry == DockerHub {
		repo = strings.TrimPrefix(repo, "library/")
	} else {
		repo = r.OriginalRegistry + "/" + repo
	}
	tag := r.Tag
	if tag == "" {
		tag = "latest"
	}
	return repo + ":" + tag
}

// RepositoryName 返回不带标签的镜像名。
func (r *Reference) RepositoryName() string {
	repo := r.Repository
	if r.OriginalRegistry == DockerHub {
		return strings.TrimPrefix(repo, "library/")
	}
	return r.OriginalRegistry + "/" + repo
}

// TagName 返回用于打标签的 tag（使用摘要拉取时默认 latest）。
func (r *Reference) TagName() string {
	if r.Tag == "" {
		return "latest"
	}
	return r.Tag
}

// String 返回便于日志展示的完整引用。
func (r *Reference) String() string {
	if r.Digest != "" {
		return r.OriginalRegistry + "/" + r.Repository + "@" + r.Digest
	}
	return r.Name()
}
