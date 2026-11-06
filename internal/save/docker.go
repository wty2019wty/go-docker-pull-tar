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
	"path/filepath"

	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

// DockerOptions 是生成 docker save 格式 tar 的参数。
type DockerOptions struct {
	OutputPath   string
	RefName      string
	Repository   string
	Tag          string
	ConfigDigest string
	ConfigBytes  []byte
	Layers       []LayerSource
	TempDir      string
	// Progress 上报打包进度，可为 nil。
	Progress *ui.Progress
}

// dockerManifestEntry 对应 docker save 的 manifest.json 条目。
type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// WriteDockerTar 生成可供 docker load 导入的 tar 包。
func WriteDockerTar(ctx context.Context, opts DockerOptions) error {
	configHex, err := digestHex(opts.ConfigDigest)
	if err != nil {
		return err
	}
	configName := configHex + ".json"

	if opts.TempDir == "" {
		opts.TempDir = os.TempDir()
	}
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o700); err != nil {
		return err
	}

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

	// 1. 写入镜像配置。
	if err := writeTarFile(tw, configName, int64(len(opts.ConfigBytes))); err != nil {
		return err
	}
	if _, err := tw.Write(opts.ConfigBytes); err != nil {
		return err
	}

	// 2. 计算旧版 layer ID 链，并逐层解压写入。
	ids := make([]string, len(opts.Layers))
	parent := ""
	for i, layer := range opts.Layers {
		sum := sha256.Sum256([]byte(parent + "\n" + layer.Digest + "\n"))
		id := hex.EncodeToString(sum[:])
		ids[i] = id
		parent = id
	}

	tarLayers := make([]string, 0, len(opts.Layers))
	idxs := progressLayers(opts.Progress, opts.Layers)
	for i, layer := range opts.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		unpacked := filepath.Join(opts.TempDir, fmt.Sprintf("unpack-%d-%s.tar", i, ids[i][:8]))
		_ = os.Remove(unpacked)
		if _, _, err := unpackLayer(ctx, layer, unpacked, layerHook(opts.Progress, idxs, i)); err != nil {
			_ = os.Remove(unpacked)
			return err
		}
		fi, err := os.Stat(unpacked)
		if err != nil {
			_ = os.Remove(unpacked)
			return err
		}

		layerDir := ids[i]
		if err := writeTarFile(tw, layerDir+"/layer.tar", fi.Size()); err != nil {
			_ = os.Remove(unpacked)
			return err
		}
		in, err := os.Open(unpacked)
		if err != nil {
			_ = os.Remove(unpacked)
			return err
		}
		_, copyErr := copyWithContext(ctx, tw, in)
		in.Close()
		_ = os.Remove(unpacked)
		if copyErr != nil {
			return copyErr
		}

		legacy := map[string]interface{}{"id": layerDir}
		if i > 0 {
			legacy["parent"] = ids[i-1]
		} else {
			legacy["parent"] = nil
		}
		legacyJSON, _ := json.Marshal(legacy)
		if err := writeTarFile(tw, layerDir+"/json", int64(len(legacyJSON))); err != nil {
			return err
		}
		if _, err := tw.Write(legacyJSON); err != nil {
			return err
		}
		if err := writeTarFile(tw, layerDir+"/VERSION", 3); err != nil {
			return err
		}
		if _, err := tw.Write([]byte("1.0")); err != nil {
			return err
		}

		markLayerDone(opts.Progress, idxs, i)
		tarLayers = append(tarLayers, layerDir+"/layer.tar")
	}

	// 3. 写入 manifest.json。
	manifest := []dockerManifestEntry{{
		Config:   configName,
		RepoTags: []string{opts.RefName},
		Layers:   tarLayers,
	}}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, "manifest.json", int64(len(manifestJSON))); err != nil {
		return err
	}
	if _, err := tw.Write(manifestJSON); err != nil {
		return err
	}

	// 4. 写入旧版 repositories 文件（兼容老版本 docker）。
	repo := opts.Repository
	if repo == "" {
		repo = opts.RefName
	}
	repositories := map[string]map[string]string{repo: {opts.Tag: parent}}
	repositoriesJSON, err := json.Marshal(repositories)
	if err != nil {
		return err
	}
	if err := writeTarFile(tw, "repositories", int64(len(repositoriesJSON))); err != nil {
		return err
	}
	if _, err := tw.Write(repositoriesJSON); err != nil {
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

func copyWithContext(ctx context.Context, dst *tar.Writer, src *os.File) (int64, error) {
	buf := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				return total, nil
			}
			return total, err
		}
	}
}
