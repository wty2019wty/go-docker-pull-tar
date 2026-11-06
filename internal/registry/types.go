// Package registry 实现 Docker Registry HTTP API V2 客户端。
package registry

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 常见清单媒体类型。
const (
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerSchema1      = "application/vnd.docker.distribution.manifest.v1+prettyjws"
)

// AcceptHeader 是请求清单时携带的 Accept 头。
var AcceptHeader = strings.Join([]string{
	MediaTypeDockerManifest,
	MediaTypeDockerManifestList,
	MediaTypeOCIManifest,
	MediaTypeOCIIndex,
}, ", ")

// Descriptor 对应 OCI 内容描述符。
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	URLs        []string          `json:"urls,omitempty"`
	Platform    *Platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Platform 描述镜像目标平台。
type Platform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

// String 返回 linux/amd64 形式的字符串。
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// Manifest 同时兼容镜像清单与多平台索引。
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// IsIndex 判断是否为多平台索引。
func (m *Manifest) IsIndex() bool {
	if len(m.Manifests) > 0 {
		return true
	}
	return m.MediaType == MediaTypeDockerManifestList || m.MediaType == MediaTypeOCIIndex
}

// ImageConfig 是镜像配置 JSON 中本工具关心的字段。
type ImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// ParseManifest 解析清单。
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析清单 JSON 失败: %w", err)
	}
	if m.MediaType == MediaTypeDockerSchema1 {
		return nil, fmt.Errorf("不支持已废弃的 Docker Schema 1 镜像")
	}
	if m.SchemaVersion != 2 && m.SchemaVersion != 0 {
		return nil, fmt.Errorf("不支持的清单版本: %d", m.SchemaVersion)
	}
	if len(m.Manifests) == 0 && len(m.Layers) == 0 {
		return nil, fmt.Errorf("清单中既没有 manifests 也没有 layers")
	}
	return &m, nil
}

// IsLayerGzip 判断层是否为 gzip 压缩。
func IsLayerGzip(mediaType string) bool {
	return strings.Contains(mediaType, "gzip") ||
		mediaType == "application/vnd.docker.image.rootfs.diff.tar.gzip" ||
		mediaType == "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip" ||
		mediaType == "application/vnd.oci.image.layer.v1.tar+gzip"
}
