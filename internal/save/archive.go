// Package save 将下载好的镜像层打包为 docker load 或 OCI 布局。
package save

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LayerSource 描述一个本地层文件。
type LayerSource struct {
	Digest    string
	MediaType string
	BlobPath  string
	DiffID    string
	Size      int64
}

// ctxReader 在每次读取时检查取消信号。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}

// writeTarFile 向 tar 写入一个常规文件头。
func writeTarFile(tw *tar.Writer, name string, size int64) error {
	hdr := &tar.Header{
		Name:     filepath.ToSlash(name),
		Mode:     0o644,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}
	return tw.WriteHeader(hdr)
}

// writeTarDir 向 tar 写入一个目录头。
func writeTarDir(tw *tar.Writer, name string) error {
	hdr := &tar.Header{
		Name:     filepath.ToSlash(name) + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now(),
	}
	return tw.WriteHeader(hdr)
}

// compressionType 根据文件头判断压缩格式。
type compressionType int

const (
	compressionNone compressionType = iota
	compressionGzip
	compressionZstd
)

func detectCompression(r *bufio.Reader) (compressionType, error) {
	magic, err := r.Peek(4)
	if err != nil {
		if err == io.EOF {
			return compressionNone, fmt.Errorf("文件为空")
		}
		// 不足 4 字节时按未压缩处理。
		return compressionNone, nil
	}
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		return compressionGzip, nil
	case magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		return compressionZstd, nil
	default:
		return compressionNone, nil
	}
}

// unpackLayer 将层解压为未压缩 tar，并返回其 diffID。
func unpackLayer(ctx context.Context, src LayerSource, dst string) (string, int64, error) {
	in, err := os.Open(src.BlobPath)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()

	buffered := bufio.NewReaderSize(in, 256<<10)
	ctype, err := detectCompression(buffered)
	if err != nil {
		return "", 0, err
	}

	var reader io.Reader = buffered
	switch ctype {
	case compressionGzip:
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return "", 0, fmt.Errorf("解压层失败: %w", err)
		}
		defer gz.Close()
		reader = gz
	case compressionZstd:
		return "", 0, fmt.Errorf("该镜像层使用 zstd 压缩，docker 格式暂不支持；请改用 -format oci")
	}

	out, err := os.Create(dst)
	if err != nil {
		return "", 0, err
	}

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)
	if _, err := io.Copy(writer, ctxReader{ctx: ctx, r: reader}); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return "", 0, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return "", 0, err
	}

	fi, err := os.Stat(dst)
	if err != nil {
		return "", 0, err
	}
	diffID := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if src.DiffID != "" && src.DiffID != diffID {
		_ = os.Remove(dst)
		return "", 0, fmt.Errorf("层 %s 的 diffID 校验失败: 期望 %s，实际 %s", shortDigest(src.Digest), src.DiffID, diffID)
	}
	return diffID, fi.Size(), nil
}

// copyFile 复制文件并同时校验 sha256。
func copyFile(ctx context.Context, src, dst, expectedDigest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), ctxReader{ctx: ctx, r: in}); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if expectedDigest != "" && actual != expectedDigest {
		_ = os.Remove(tmp)
		return fmt.Errorf("文件校验失败: 期望 %s，实际 %s", expectedDigest, actual)
	}
	return os.Rename(tmp, dst)
}

func digestHex(digest string) (string, error) {
	i := -1
	for idx := 0; idx < len(digest); idx++ {
		if digest[idx] == ':' {
			i = idx
			break
		}
	}
	if i <= 0 || digest[:i] != "sha256" || len(digest[i+1:]) != 64 {
		return "", fmt.Errorf("无效的 sha256 摘要: %q", digest)
	}
	if _, err := hex.DecodeString(digest[i+1:]); err != nil {
		return "", fmt.Errorf("无效的 sha256 摘要: %q", digest)
	}
	return digest[i+1:], nil
}

func shortDigest(digest string) string {
	h, err := digestHex(digest)
	if err != nil {
		return digest
	}
	return h[:12]
}
