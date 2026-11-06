package save

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/wty2019wty/go-docker-pull-tar/internal/registry"
	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

// DockerModernOptions 是生成新版 docker save 格式（OCI 布局 + manifest.json）的参数。
type DockerModernOptions struct {
	OutputPath        string
	RefName           string
	Repository        string
	Tag               string
	Platform          registry.Platform
	ManifestBytes     []byte
	ManifestDigest    string
	ManifestMediaType string
	ConfigDigest      string
	ConfigBytes       []byte
	Layers            []LayerSource
	TempDir           string
	// Progress 上报打包进度，可为 nil。
	Progress *ui.Progress
}

// dockerModernManifestItem 对应新版 docker save 的 manifest.json 条目。
type dockerModernManifestItem struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// WriteDockerModern 生成与 Docker 27+ `docker save` 一致的混合格式：
// OCI 布局（blobs/sha256/*、index.json、oci-layout）加上 legacy manifest.json。
// docker load 会读取 manifest.json；OCI 工具可以读取 index.json。
func WriteDockerModern(ctx context.Context, opts DockerModernOptions) error {
	configHex, err := digestHex(opts.ConfigDigest)
	if err != nil {
		return err
	}
	manifestHex, err := digestHex(opts.ManifestDigest)
	if err != nil {
		return err
	}
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o700); err != nil {
		return err
	}

	layerHashes := make([]string, len(opts.Layers))
	for i, layer := range opts.Layers {
		h, err := digestHex(layer.Digest)
		if err != nil {
			return err
		}
		layerHashes[i] = h
	}
	idxs := progressLayers(opts.Progress, opts.Layers)

	tmpTar := opts.OutputPath + ".tmp"
	_ = os.Remove(tmpTar)
	f, err := os.OpenFile(tmpTar, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			f.Close()
			_ = os.Remove(tmpTar)
		}
	}()

	bw := bufio.NewWriterSize(f, 1<<20)
	tw := tar.NewWriter(bw)

	// 1. 配置 Blob。
	if err := writeBlobBytes(tw, path.Join("blobs", "sha256", configHex), opts.ConfigBytes); err != nil {
		return err
	}
	// 2. 层 Blob（保持原始压缩格式，不再解压）。
	for i, layer := range opts.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeBlobFile(ctx, tw, path.Join("blobs", "sha256", layerHashes[i]), layer.BlobPath, layer.Digest, layerHook(opts.Progress, idxs, i)); err != nil {
			return err
		}
		markLayerDone(opts.Progress, idxs, i)
	}
	// 3. OCI 清单 Blob。
	if err := writeBlobBytes(tw, path.Join("blobs", "sha256", manifestHex), opts.ManifestBytes); err != nil {
		return err
	}

	// 4. OCI 入口文件。
	if err := writeBlobBytes(tw, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	index := ociIndex{
		SchemaVersion: 2,
		MediaType:     registry.MediaTypeOCIIndex,
		Manifests: []ociIndexEntry{{
			MediaType: normalizeManifestMediaType(opts.ManifestMediaType),
			Digest:    opts.ManifestDigest,
			Size:      int64(len(opts.ManifestBytes)),
			Platform:  &opts.Platform,
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": opts.RefName,
			},
		}},
	}
	indexJSON, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := writeBlobBytes(tw, "index.json", indexJSON); err != nil {
		return err
	}

	// 5. legacy manifest.json。
	layers := make([]string, 0, len(opts.Layers))
	for _, h := range layerHashes {
		layers = append(layers, path.Join("blobs", "sha256", h))
	}
	manifest := []dockerModernManifestItem{{
		Config:   path.Join("blobs", "sha256", configHex),
		RepoTags: []string{opts.RefName},
		Layers:   layers,
	}}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := writeBlobBytes(tw, "manifest.json", manifestJSON); err != nil {
		return err
	}

	// 6. 旧版 repositories。
	repo := opts.Repository
	if repo == "" {
		repo = opts.RefName
	}
	topID := ""
	if len(layerHashes) > 0 {
		topID = layerHashes[len(layerHashes)-1]
	}
	repositories := map[string]map[string]string{repo: {opts.Tag: topID}}
	repositoriesJSON, err := json.Marshal(repositories)
	if err != nil {
		return err
	}
	if err := writeBlobBytes(tw, "repositories", repositoriesJSON); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpTar, opts.OutputPath); err != nil {
		_ = os.Remove(tmpTar)
		return err
	}
	success = true
	return nil
}

func writeBlobBytes(tw *tar.Writer, name string, data []byte) error {
	if err := writeTarFile(tw, name, int64(len(data))); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// writeBlobFile 将本地 Blob 写入 tar，同时重新校验摘要。onBytes 上报读取的字节数。
func writeBlobFile(ctx context.Context, tw *tar.Writer, name, srcPath, expectedDigest string, onBytes byteHook) error {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, name, fi.Size()); err != nil {
		return err
	}
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := tw.Write(buf[:n]); err != nil {
				return err
			}
			h.Write(buf[:n])
			onBytes.add(int64(n))
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}
	actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if actual != expectedDigest {
		return fmt.Errorf("层 %s 校验失败: 实际 %s", shortDigest(expectedDigest), actual)
	}
	return nil
}

func normalizeManifestMediaType(mediaType string) string {
	if mediaType == "" {
		return registry.MediaTypeOCIManifest
	}
	return mediaType
}
