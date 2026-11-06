package save

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/topcss/go-docker-pull-tar/internal/registry"
)

// OCIOptions 是生成 OCI 镜像布局的参数。
type OCIOptions struct {
	// OutputPath 可以是目录，也可以是以 .tar 结尾的文件。
	OutputPath        string
	RefName           string
	Platform          registry.Platform
	ManifestBytes     []byte
	ManifestDigest    string
	ManifestMediaType string
	ConfigDigest      string
	ConfigBytes       []byte
	Layers            []LayerSource
	TempDir           string
}

type ociIndexEntry struct {
	MediaType   string             `json:"mediaType"`
	Digest      string             `json:"digest"`
	Size        int64              `json:"size"`
	Platform    *registry.Platform `json:"platform,omitempty"`
	Annotations map[string]string  `json:"annotations,omitempty"`
}

type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociIndexEntry `json:"manifests"`
}

type ociLayout struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

// WriteOCI 生成 OCI image layout，或以 .tar 结尾时生成布局 tar 包。
func WriteOCI(ctx context.Context, opts OCIOptions) error {
	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	if err := os.MkdirAll(opts.TempDir, 0o755); err != nil {
		return err
	}

	asTar := strings.HasSuffix(strings.ToLower(opts.OutputPath), ".tar")
	layoutDir := opts.OutputPath
	cleanupDir := ""
	if asTar {
		dir, err := os.MkdirTemp(opts.TempDir, "oci-layout-")
		if err != nil {
			return err
		}
		layoutDir = dir
		cleanupDir = dir
		defer func() {
			if cleanupDir != "" {
				_ = os.RemoveAll(cleanupDir)
			}
		}()
	} else if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		return err
	}

	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return err
	}

	// 1. 写入清单 Blob。
	if err := writeBytesVerified(ctx, blobsDir, opts.ManifestDigest, opts.ManifestBytes); err != nil {
		return err
	}
	// 2. 写入配置 Blob。
	if err := writeBytesVerified(ctx, blobsDir, opts.ConfigDigest, opts.ConfigBytes); err != nil {
		return err
	}
	// 3. 复制层 Blob。
	for _, layer := range opts.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := digestHex(layer.Digest)
		if err != nil {
			return err
		}
		dst := filepath.Join(blobsDir, h)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		if err := copyFile(ctx, layer.BlobPath, dst, layer.Digest); err != nil {
			return err
		}
	}

	// 4. 写入 index.json。
	mt := opts.ManifestMediaType
	if mt == "" {
		mt = registry.MediaTypeDockerManifest
	}
	index := ociIndex{
		SchemaVersion: 2,
		MediaType:     registry.MediaTypeOCIIndex,
		Manifests: []ociIndexEntry{{
			MediaType: mt,
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
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), indexJSON, 0o644); err != nil {
		return err
	}
	layoutJSON, _ := json.Marshal(ociLayout{ImageLayoutVersion: "1.0.0"})
	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"), layoutJSON, 0o644); err != nil {
		return err
	}

	// 5. 需要时打包为 tar。
	if asTar {
		if err := tarDir(ctx, layoutDir, opts.OutputPath); err != nil {
			return err
		}
	}
	return nil
}

func writeBytesVerified(ctx context.Context, dir, digest string, data []byte) error {
	h, err := digestHex(digest)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if actual := "sha256:" + hex.EncodeToString(sum[:]); actual != digest {
		return fmt.Errorf("内容校验失败: 期望 %s，实际 %s", digest, actual)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, h), data, 0o644)
}

// tarDir 将目录打包为 tar，路径保持相对形式。
func tarDir(ctx context.Context, srcDir, output string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	tmp := output + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			f.Close()
			_ = os.Remove(tmp)
		}
	}()

	bw := bufio.NewWriterSize(f, 1<<20)
	tw := tar.NewWriter(bw)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			hdr := &tar.Header{
				Name:     rel + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
				ModTime:  info.ModTime(),
			}
			return tw.WriteHeader(hdr)
		}
		hdr := &tar.Header{
			Name:     rel,
			Mode:     0o644,
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
			ModTime:  info.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := copyWithContext(ctx, tw, file)
		file.Close()
		return copyErr
	})
	if err != nil {
		tw.Close()
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
	if err := os.Rename(tmp, output); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	success = true
	return nil
}
