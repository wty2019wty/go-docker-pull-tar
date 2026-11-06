package save

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/topcss/go-docker-pull-tar/internal/registry"
)

func makeTestLayer(t *testing.T) (layerTar, blob []byte, diffID, blobDigest string) {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("hello from go-docker-pull-tar")
	if err := tw.WriteHeader(&tar.Header{
		Name: "hello.txt",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layerTar = tarBuf.Bytes()
	sum := sha256.Sum256(layerTar)
	diffID = "sha256:" + hex.EncodeToString(sum[:])

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(layerTar); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	blob = gzBuf.Bytes()
	sum = sha256.Sum256(blob)
	blobDigest = "sha256:" + hex.EncodeToString(sum[:])
	return
}

func makeTestConfig(t *testing.T, diffID string) ([]byte, string) {
	t.Helper()
	cfg := fmt.Sprintf(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[%q]}}`, diffID)
	sum := sha256.Sum256([]byte(cfg))
	return []byte(cfg), "sha256:" + hex.EncodeToString(sum[:])
}

func TestWriteDockerTar(t *testing.T) {
	dir := t.TempDir()
	layerTar, blob, diffID, blobDigest := makeTestLayer(t)
	configBytes, configDigest := makeTestConfig(t, diffID)

	blobPath := filepath.Join(dir, "layer.blob")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "image.tar")
	err := WriteDockerTar(context.Background(), DockerOptions{
		OutputPath:   out,
		RefName:      "test/app:latest",
		Repository:   "test/app",
		Tag:          "latest",
		ConfigDigest: configDigest,
		ConfigBytes:  configBytes,
		Layers: []LayerSource{{
			Digest:   blobDigest,
			BlobPath: blobPath,
			DiffID:   diffID,
		}},
		TempDir: dir,
	})
	if err != nil {
		t.Fatalf("WriteDockerTar() error = %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = data
	}

	var manifests []dockerManifestEntry
	if err := json.Unmarshal(entries["manifest.json"], &manifests); err != nil {
		t.Fatalf("manifest.json 无效: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifest 条目数 = %d", len(manifests))
	}
	if manifests[0].RepoTags[0] != "test/app:latest" {
		t.Errorf("RepoTags = %v", manifests[0].RepoTags)
	}
	layerPath := manifests[0].Layers[0]
	gotLayer, ok := entries[layerPath]
	if !ok {
		t.Fatalf("缺少层文件 %q", layerPath)
	}
	if !bytes.Equal(gotLayer, layerTar) {
		t.Fatal("layer.tar 内容与未压缩层不一致")
	}
	sum := sha256.Sum256(gotLayer)
	if "sha256:"+hex.EncodeToString(sum[:]) != diffID {
		t.Fatal("layer.tar 的 diffID 不一致")
	}
	if _, ok := entries[manifests[0].Config]; !ok {
		t.Fatal("manifest.json 引用的配置文件不存在")
	}
	if _, ok := entries["repositories"]; !ok {
		t.Fatal("缺少 repositories 文件")
	}
}

func TestWriteDockerModern(t *testing.T) {
	dir := t.TempDir()
	layerTar, blob, diffID, blobDigest := makeTestLayer(t)
	configBytes, configDigest := makeTestConfig(t, diffID)
	blobPath := filepath.Join(dir, "layer.blob")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	manifestBytes := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"digest":%q,"size":%d},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		registry.MediaTypeOCIManifest,
		configDigest, len(configBytes),
		"application/vnd.oci.image.layer.v1.tar+gzip",
		blobDigest, len(blob)))
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])

	out := filepath.Join(dir, "modern.tar")
	err := WriteDockerModern(context.Background(), DockerModernOptions{
		OutputPath:        out,
		RefName:           "test/app:latest",
		Repository:        "test/app",
		Tag:               "latest",
		Platform:          registry.Platform{OS: "linux", Architecture: "amd64"},
		ManifestBytes:     manifestBytes,
		ManifestDigest:    manifestDigest,
		ManifestMediaType: registry.MediaTypeOCIManifest,
		ConfigDigest:      configDigest,
		ConfigBytes:       configBytes,
		Layers:            []LayerSource{{Digest: blobDigest, BlobPath: blobPath}},
		TempDir:           dir,
	})
	if err != nil {
		t.Fatalf("WriteDockerModern() error = %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	entries := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = data
	}

	// 模拟 docker load：读取 manifest.json，校验配置与层路径、数量、diffID。
	var manifests []dockerModernManifestItem
	if err := json.Unmarshal(entries["manifest.json"], &manifests); err != nil {
		t.Fatalf("manifest.json 无效: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("manifest 条目数 = %d", len(manifests))
	}
	gotConfig, ok := entries[manifests[0].Config]
	if !ok || !bytes.Equal(gotConfig, configBytes) {
		t.Fatal("manifest.json 指向的配置不存在或不一致")
	}
	if len(manifests[0].Layers) != 1 {
		t.Fatalf("层数量 = %d", len(manifests[0].Layers))
	}
	layerData, ok := entries[manifests[0].Layers[0]]
	if !ok {
		t.Fatalf("缺少层 %q", manifests[0].Layers[0])
	}
	if sha256DigestBytes(layerData) != blobDigest {
		t.Fatal("层 Blob 摘要与路径不一致")
	}
	// 解压后应等于原始 layer.tar。
	gz, err := gzip.NewReader(bytes.NewReader(layerData))
	if err != nil {
		t.Fatal(err)
	}
	uncompressed, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(uncompressed, layerTar) {
		t.Fatal("层 Blob 解压内容与原始 layer.tar 不一致")
	}
	if _, ok := entries["oci-layout"]; !ok {
		t.Fatal("缺少 oci-layout")
	}
	if _, ok := entries["index.json"]; !ok {
		t.Fatal("缺少 index.json")
	}
}

func sha256DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestWriteOCI(t *testing.T) {
	dir := t.TempDir()
	_, blob, diffID, blobDigest := makeTestLayer(t)
	configBytes, configDigest := makeTestConfig(t, diffID)
	blobPath := filepath.Join(dir, "layer.blob")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	manifestBytes := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"digest":%q},"layers":[{"digest":%q}]}`,
		registry.MediaTypeOCIManifest, configDigest, blobDigest))
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])

	layout := filepath.Join(dir, "layout")
	err := WriteOCI(context.Background(), OCIOptions{
		OutputPath:        layout,
		RefName:           "test/app:latest",
		Platform:          registry.Platform{OS: "linux", Architecture: "amd64"},
		ManifestBytes:     manifestBytes,
		ManifestDigest:    manifestDigest,
		ManifestMediaType: registry.MediaTypeOCIManifest,
		ConfigDigest:      configDigest,
		ConfigBytes:       configBytes,
		Layers: []LayerSource{{
			Digest:   blobDigest,
			BlobPath: blobPath,
		}},
		TempDir: dir,
	})
	if err != nil {
		t.Fatalf("WriteOCI() error = %v", err)
	}

	for _, name := range []string{"oci-layout", "index.json"} {
		if _, err := os.Stat(filepath.Join(layout, name)); err != nil {
			t.Fatalf("缺少 %s: %v", name, err)
		}
	}
	for _, digest := range []string{configDigest, blobDigest, manifestDigest} {
		h := digest[len("sha256:"):]
		if _, err := os.Stat(filepath.Join(layout, "blobs", "sha256", h)); err != nil {
			t.Fatalf("缺少 blob %s: %v", digest, err)
		}
	}

	var index ociIndex
	data, err := os.ReadFile(filepath.Join(layout, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != manifestDigest {
		t.Fatalf("index.json 内容错误: %+v", index)
	}
}

func TestWriteOCITar(t *testing.T) {
	dir := t.TempDir()
	_, blob, diffID, blobDigest := makeTestLayer(t)
	configBytes, configDigest := makeTestConfig(t, diffID)
	blobPath := filepath.Join(dir, "layer.blob")
	if err := os.WriteFile(blobPath, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestBytes := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"digest":%q},"layers":[{"digest":%q}]}`,
		registry.MediaTypeOCIManifest, configDigest, blobDigest))
	sum := sha256.Sum256(manifestBytes)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])

	out := filepath.Join(dir, "layout.tar")
	if err := WriteOCI(context.Background(), OCIOptions{
		OutputPath:        out,
		RefName:           "test/app:latest",
		Platform:          registry.Platform{OS: "linux", Architecture: "amd64"},
		ManifestBytes:     manifestBytes,
		ManifestDigest:    manifestDigest,
		ManifestMediaType: registry.MediaTypeOCIManifest,
		ConfigDigest:      configDigest,
		ConfigBytes:       configBytes,
		Layers:            []LayerSource{{Digest: blobDigest, BlobPath: blobPath}},
		TempDir:           dir,
	}); err != nil {
		t.Fatalf("WriteOCI() error = %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "index.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("OCI tar 中缺少 index.json")
	}
}
