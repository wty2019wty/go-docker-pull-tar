// go-docker-pull-tar 是一个无需 Docker 环境的纯 Go 镜像拉取工具。
//
// 它直接调用 Docker Registry HTTP API V2，支持多线程分片下载、
// 断点续传、代理、登录认证、指定架构，并输出 docker load 可用的
// tar 包或 OCI 镜像布局。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wty2019wty/go-docker-pull-tar/internal/download"
	"github.com/wty2019wty/go-docker-pull-tar/internal/reference"
	"github.com/wty2019wty/go-docker-pull-tar/internal/registry"
	"github.com/wty2019wty/go-docker-pull-tar/internal/save"
	"github.com/wty2019wty/go-docker-pull-tar/internal/transport"
	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

// Version 是程序版本号，默认为 dev，正式发布时由构建脚本或 CI 通过
// -ldflags "-X main.Version=vX.Y.Z" 注入与 GitHub Release 一致的版本。
var Version = "dev"

type cliOptions struct {
	image          string
	platform       string
	osName         string
	arch           string
	variant        string
	listPlatforms  bool
	username       string
	password       string
	passwordStdin  bool
	tagName        string
	registryMirror string
	proxy          string
	dns            string
	insecure       bool
	plainHTTP      bool
	workers        int
	chunkSize      int64
	minChunkSize   int64
	retries        int
	format         string
	output         string
	cacheDir       string
	keepCache      bool
	quiet          bool
	verbose        bool
	version        bool
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	var opts cliOptions
	fs := flag.NewFlagSet("go-docker-pull-tar", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fs.StringVar(&opts.image, "image", "", "镜像名称，例如 nginx:latest")
	fs.StringVar(&opts.image, "i", "", "镜像名称（-image 的简写）")
	fs.StringVar(&opts.platform, "platform", "", "目标平台，例如 linux/amd64、linux/arm/v7")
	fs.StringVar(&opts.osName, "os", "", "目标操作系统，默认 linux")
	fs.StringVar(&opts.arch, "arch", "", "目标架构，例如 amd64、arm64")
	fs.StringVar(&opts.arch, "a", "", "目标架构（-arch 的简写）")
	fs.StringVar(&opts.variant, "variant", "", "架构变体，例如 v7")
	fs.BoolVar(&opts.listPlatforms, "list-platforms", false, "列出镜像支持的平台后退出")
	fs.StringVar(&opts.username, "username", "", "仓库用户名")
	fs.StringVar(&opts.username, "u", "", "仓库用户名（-username 的简写）")
	fs.StringVar(&opts.password, "password", "", "仓库密码")
	fs.StringVar(&opts.password, "p", "", "仓库密码（-password 的简写）")
	fs.BoolVar(&opts.passwordStdin, "password-stdin", false, "从标准输入读取密码")
	fs.StringVar(&opts.tagName, "tag-name", "", "写入 tar 包的镜像名，默认自动生成")
	fs.StringVar(&opts.registryMirror, "registry", "", "镜像站地址，例如 docker.1ms.run")
	fs.StringVar(&opts.registryMirror, "r", "", "镜像站地址（-registry 的简写）")
	fs.StringVar(&opts.proxy, "proxy", "", "代理地址：http://、https://、socks5://")
	fs.StringVar(&opts.dns, "dns", "", "自定义 DNS 服务器，逗号分隔（如 223.5.5.5,8.8.8.8），默认自动检测")
	fs.BoolVar(&opts.insecure, "insecure", false, "跳过 TLS 证书校验")
	fs.BoolVar(&opts.plainHTTP, "plain-http", false, "使用 HTTP 访问仓库（本地仓库调试用）")
	fs.IntVar(&opts.workers, "workers", 8, "并发下载连接数")
	fs.IntVar(&opts.workers, "w", 8, "并发下载连接数（-workers 的简写）")
	var chunkSizeStr, minChunkSizeStr string
	fs.StringVar(&chunkSizeStr, "chunk-size", "8MiB", "大层分片大小，0 表示禁用分片")
	fs.StringVar(&minChunkSizeStr, "min-chunk-size", "16MiB", "启用分片的最小层大小")
	fs.IntVar(&opts.retries, "retries", 8, "单个请求的最大重试次数")
	fs.StringVar(&opts.format, "format", "docker", "输出格式：docker、docker-legacy 或 oci")
	fs.StringVar(&opts.output, "output", "", "输出文件或目录")
	fs.StringVar(&opts.output, "o", "", "输出文件或目录（-output 的简写）")
	fs.StringVar(&opts.cacheDir, "cache-dir", "", "下载缓存目录（断点续传使用）")
	fs.BoolVar(&opts.keepCache, "keep-cache", false, "下载完成后保留缓存")
	fs.BoolVar(&opts.quiet, "quiet", false, "静默模式，不显示进度")
	fs.BoolVar(&opts.quiet, "q", false, "静默模式（-quiet 的简写）")
	fs.BoolVar(&opts.verbose, "verbose", false, "输出详细调试日志")
	fs.BoolVar(&opts.version, "version", false, "显示版本信息")
	fs.BoolVar(&opts.version, "V", false, "显示版本信息")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `go-docker-pull-tar %s - 无需 Docker 环境的镜像拉取工具

用法:
  gdockerpull [选项] <镜像>
  gdockerpull pull [选项] <镜像>

示例:
  gdockerpull nginx:latest
  gdockerpull -i nginx:1.25 -a arm64
  gdockerpull -i harbor.example.com/team/app:v1 -u admin -p secret
  gdockerpull -i nginx:latest -registry docker.1ms.run -proxy socks5://127.0.0.1:1080
  gdockerpull -i ghcr.io/owner/repo:tag -platform linux/amd64 -workers 16
  gdockerpull -i nginx:latest -dns 223.5.5.5,8.8.8.8
  gdockerpull -i nginx:latest -format oci -o ./oci-out

选项:
`, Version)
		fs.PrintDefaults()
	}

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "pull" {
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if opts.version {
		fmt.Printf("go-docker-pull-tar %s\n", Version)
		return 0
	}
	if opts.image == "" && fs.NArg() > 0 {
		opts.image = fs.Arg(0)
	}

	chunkSize, err := parseSize(chunkSizeStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 无效的 -chunk-size: %v\n", err)
		return 2
	}
	minChunk, err := parseSize(minChunkSizeStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: 无效的 -min-chunk-size: %v\n", err)
		return 2
	}
	opts.chunkSize = chunkSize
	opts.minChunkSize = minChunk
	opts.format = strings.ToLower(strings.TrimSpace(opts.format))
	if opts.format != "docker" && opts.format != "docker-legacy" && opts.format != "oci" {
		fmt.Fprintf(os.Stderr, "错误: 无效的 -format: %q（支持 docker、docker-legacy、oci）\n", opts.format)
		return 2
	}
	if opts.workers <= 0 {
		fmt.Fprintln(os.Stderr, "错误: -workers 必须大于 0")
		return 2
	}
	if opts.workers > 128 {
		opts.workers = 128
	}
	if opts.retries < 0 {
		opts.retries = 0
	}

	u := ui.New(opts.quiet, opts.verbose)

	if opts.image == "" {
		if !stdinIsTerminal() {
			fmt.Fprintln(os.Stderr, "错误: 请通过 -i/--image 指定镜像名称")
			return 2
		}
		line, ok := promptLine("请输入镜像名称（例如 nginx:latest）: ")
		if !ok || strings.TrimSpace(line) == "" {
			u.Errorf("错误: 镜像名称不能为空")
			return 2
		}
		opts.image = strings.TrimSpace(line)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	listed, err := run(ctx, opts, u)
	if err != nil {
		if ctx.Err() != nil {
			u.Errorf("\n警告: 已中断，已下载的分片保留在缓存目录中；重新运行相同命令即可断点续传。")
			return 130
		}
		u.Errorf("错误: %v", err)
		return 1
	}
	if listed {
		return 0
	}
	return 0
}

func run(ctx context.Context, opts cliOptions, u *ui.UI) (bool, error) {
	ref, err := reference.Parse(opts.image)
	if err != nil {
		return false, err
	}
	if opts.registryMirror != "" {
		lower := strings.ToLower(opts.registryMirror)
		if strings.HasPrefix(lower, "http://") {
			opts.plainHTTP = true
		}
		ref.UseRegistry(opts.registryMirror)
	}

	creds, err := resolveCredentials(opts, ref, u)
	if err != nil {
		return false, err
	}

	httpClient, err := transport.New(transport.Options{
		Proxy:    opts.proxy,
		DNS:      opts.dns,
		Insecure: opts.insecure,
		Log:      u.Infof,
		Verbose:  u.Verbosef,
	})
	if err != nil {
		return false, err
	}

	rc := registry.New(registry.Options{
		Registry:    ref.Registry,
		Repository:  ref.Repository,
		Credentials: creds,
		HTTPClient:  httpClient,
		UserAgent:   "go-docker-pull-tar/" + Version,
		PlainHTTP:   opts.plainHTTP,
		Verbose:     u.Verbosef,
		Log:         u.Infof,
	})

	u.Infof("仓库地址: %s", ref.Registry)
	u.Infof("仓库路径: %s", ref.Repository)
	u.Infof("引用: %s", ref.Reference())

	u.Infof("正在获取镜像清单: %s/manifests/%s", ref.Registry, ref.Reference())
	stopWatch := u.StartStallWatch("获取镜像清单", 15*time.Second)
	start := time.Now()
	manifestBytes, mediaType, manifestDigest, err := rc.FetchManifest(ctx, ref.Reference())
	stopWatch()
	if err != nil {
		return false, err
	}
	u.Infof("已获取清单: %s（%s，耗时 %s）", mediaType, ui.FormatBytes(int64(len(manifestBytes))), time.Since(start).Round(time.Millisecond))
	if ref.Digest != "" && manifestDigest != ref.Digest {
		return false, fmt.Errorf("清单摘要不匹配: 期望 %s，实际 %s", ref.Digest, manifestDigest)
	}
	manifest, err := registry.ParseManifest(manifestBytes)
	if err != nil {
		return false, err
	}

	want, platformExplicit, err := resolvePlatform(opts)
	if err != nil {
		return false, err
	}

	var platform registry.Platform
	wasIndex := manifest.IsIndex()
	if manifest.IsIndex() {
		u.Infof("该镜像是多平台索引，共 %d 个平台: %s",
			len(registry.Platforms(manifest.Manifests)), strings.Join(registry.Platforms(manifest.Manifests), ", "))
		if opts.listPlatforms {
			printPlatforms(u, manifest.Manifests)
			return true, nil
		}
		desc, ok := registry.SelectManifest(manifest.Manifests, want)
		if !ok {
			available := registry.Platforms(manifest.Manifests)
			if !platformExplicit && stdinIsTerminal() {
				chosen, perr := promptPlatform(u, available, want)
				if perr != nil {
					return false, perr
				}
				want = chosen
				desc, ok = registry.SelectManifest(manifest.Manifests, want)
			}
			if !ok {
				return false, fmt.Errorf("镜像 %s 不支持平台 %s\n可用平台: %s\n可使用 -platform/-arch 指定架构，或添加 -list-platforms 查看",
					ref.Name(), want.String(), strings.Join(available, ", "))
			}
		}
		u.Infof("已选择平台 %s -> %s", want.String(), desc.Digest)

		u.Infof("正在获取平台清单: %s", desc.Digest)
		stopWatch = u.StartStallWatch("获取平台清单", 15*time.Second)
		start = time.Now()
		childBytes, childMedia, err := rc.FetchManifestDigest(ctx, desc.Digest)
		stopWatch()
		if err != nil {
			return false, err
		}
		child, err := registry.ParseManifest(childBytes)
		if err != nil {
			return false, err
		}
		u.Infof("已获取平台清单: %d 个层，%s（耗时 %s）", len(child.Layers), ui.FormatBytes(totalLayerSize(child.Layers)), time.Since(start).Round(time.Millisecond))
		if child.IsIndex() {
			return false, fmt.Errorf("暂不支持嵌套的多平台索引")
		}
		manifest = child
		manifestBytes = childBytes
		mediaType = childMedia
		manifestDigest = desc.Digest
		platform = want
		if desc.Platform != nil {
			platform = *desc.Platform
		}
	}

	if len(manifest.Layers) == 0 {
		return false, fmt.Errorf("清单中没有任何层")
	}
	if manifest.Config.Digest == "" {
		return false, fmt.Errorf("清单中缺少配置对象")
	}

	u.Infof("正在获取镜像配置: %s", manifest.Config.Digest)
	stopWatch = u.StartStallWatch("获取镜像配置", 15*time.Second)
	start = time.Now()
	configBytes, err := rc.FetchBlob(ctx, manifest.Config.Digest, 64<<20)
	stopWatch()
	if err != nil {
		return false, err
	}
	u.Infof("已获取镜像配置: %s（耗时 %s）", ui.FormatBytes(int64(len(configBytes))), time.Since(start).Round(time.Millisecond))
	var imageConfig registry.ImageConfig
	if err := json.Unmarshal(configBytes, &imageConfig); err != nil {
		return false, fmt.Errorf("解析镜像配置失败: %w", err)
	}

	if platform.OS == "" {
		platform = registry.Platform{
			OS:           imageConfig.OS,
			Architecture: imageConfig.Architecture,
			Variant:      imageConfig.Variant,
		}
		if platform.OS == "" {
			platform = want
		}
	}

	if opts.listPlatforms {
		u.Printf("该镜像不是多平台索引，实际平台: %s", platform.String())
		return true, nil
	}

	if platformExplicit && !wasIndex && !registry.Match(want, platform) {
		u.Printf("警告: 该镜像是单平台镜像，实际平台为 %s，与指定平台 %s 不同，仍继续下载。",
			platform.String(), want.String())
	}
	if platform.Architecture != "" && imageConfig.Architecture != "" && !strings.EqualFold(platform.Architecture, imageConfig.Architecture) {
		u.Printf("警告: 配置中的架构为 %s，与清单平台 %s 不一致。", imageConfig.Architecture, platform.String())
	}

	base := safeBaseName(ref, platform)
	outputPath, err := resolveOutput(opts.output, opts.format, base)
	if err != nil {
		return false, err
	}
	cacheDir := opts.cacheDir
	if cacheDir == "" {
		cacheDir = defaultCacheDir(outputPath)
	}

	u.Infof("go-docker-pull-tar %s", Version)
	u.Infof("镜像: %s", ref.Name())
	u.Infof("仓库: %s/%s", ref.Registry, ref.Repository)
	u.Infof("平台: %s", platform.String())
	u.Infof("层数: %d，压缩大小: %s", len(manifest.Layers), ui.FormatBytes(totalLayerSize(manifest.Layers)))
	u.Infof("输出: %s", outputPath)
	u.Infof("缓存: %s", cacheDir)

	err = pullAndSave(ctx, opts, u, ref, rc, httpClient, manifest, manifestBytes, mediaType, manifestDigest, configBytes, imageConfig, platform, outputPath, cacheDir)
	return false, err
}

func pullAndSave(
	ctx context.Context,
	opts cliOptions,
	u *ui.UI,
	ref *reference.Reference,
	rc *registry.Client,
	httpClient *http.Client,
	manifest *registry.Manifest,
	manifestBytes []byte,
	mediaType string,
	manifestDigest string,
	configBytes []byte,
	imageConfig registry.ImageConfig,
	platform registry.Platform,
	outputPath string,
	cacheDir string,
) error {
	progress := ui.NewProgress(u)
	dl, err := download.New(download.Options{
		Client:         rc,
		HTTPClient:     httpClient,
		CacheDir:       cacheDir,
		ManifestDigest: manifestDigest,
		Platform:       platform.String(),
		Workers:        opts.workers,
		ChunkSize:      opts.chunkSize,
		MinChunkSize:   opts.minChunkSize,
		Retries:        opts.retries,
		Progress:       progress,
		UI:             u,
	})
	if err != nil {
		return err
	}

	// 将配置对象写入缓存，供打包阶段读取。
	configPath, err := download.ObjectPath(cacheDir, manifest.Config.Digest)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(configPath, configBytes); err != nil {
		return fmt.Errorf("写入配置对象失败: %w", err)
	}

	blobs := make([]download.Blob, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		blobs = append(blobs, download.Blob{
			Digest:    l.Digest,
			Size:      l.Size,
			URLs:      l.URLs,
			MediaType: l.MediaType,
		})
	}

	u.Infof("开始下载层数据（%d 个层，并发 %d）...", len(blobs), opts.workers)
	progress.Start()
	err = dl.Download(ctx, blobs)
	progress.Stop()
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}

	layerSources := make([]save.LayerSource, 0, len(manifest.Layers))
	for i, l := range manifest.Layers {
		p, err := download.ObjectPath(cacheDir, l.Digest)
		if err != nil {
			return err
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("层 %s 的本地文件不存在: %w", l.Digest, err)
		}
		diffID := ""
		if i < len(imageConfig.RootFS.DiffIDs) {
			diffID = imageConfig.RootFS.DiffIDs[i]
		}
		layerSources = append(layerSources, save.LayerSource{
			Digest:    l.Digest,
			MediaType: l.MediaType,
			BlobPath:  p,
			DiffID:    diffID,
			Size:      l.Size,
		})
	}
	if len(imageConfig.RootFS.DiffIDs) != len(manifest.Layers) {
		msg := fmt.Sprintf("配置中的 diffID 数量 (%d) 与层数 (%d) 不一致", len(imageConfig.RootFS.DiffIDs), len(manifest.Layers))
		if opts.format == "oci" {
			u.Printf("警告: %s，OCI 格式仍会原样保留层，但不保证可被其他工具导入。", msg)
			for i := range layerSources {
				layerSources[i].DiffID = ""
			}
		} else {
			return fmt.Errorf("%s，无法生成 docker 格式；可改用 -format oci 保留原始内容", msg)
		}
	}

	refName := opts.tagName
	if refName == "" {
		refName = ref.Name()
	}

	u.Infof("下载完成，开始生成 %s 格式...", opts.format)
	if opts.format == "docker" && hasZstdLayer(manifest.Layers) {
		u.Printf("警告: 该镜像包含 zstd 压缩层；新版 docker 格式会原样保留压缩层，需要 Docker 27+ 才能导入。")
		u.Printf("   旧版 Docker 请改用 -format oci，或使用 docker-legacy（会解压，但旧版 docker-legacy 不支持 zstd）。")
	}
	packProgress := ui.NewProgress(u)
	packProgress.SetLabel("打包")
	packProgress.Start()
	switch opts.format {
	case "docker":
		err = save.WriteDockerModern(ctx, save.DockerModernOptions{
			OutputPath:        outputPath,
			RefName:           refName,
			Repository:        ref.Repository,
			Tag:               ref.TagName(),
			Platform:          platform,
			ManifestBytes:     manifestBytes,
			ManifestDigest:    manifestDigest,
			ManifestMediaType: mediaType,
			ConfigDigest:      manifest.Config.Digest,
			ConfigBytes:       configBytes,
			Layers:            layerSources,
			TempDir:           filepath.Join(cacheDir, "tmp"),
			Progress:          packProgress,
		})
	case "docker-legacy":
		err = save.WriteDockerTar(ctx, save.DockerOptions{
			OutputPath:   outputPath,
			RefName:      refName,
			Repository:   ref.Repository,
			Tag:          ref.TagName(),
			ConfigDigest: manifest.Config.Digest,
			ConfigBytes:  configBytes,
			Layers:       layerSources,
			TempDir:      filepath.Join(cacheDir, "tmp"),
			Progress:     packProgress,
		})
	case "oci":
		err = save.WriteOCI(ctx, save.OCIOptions{
			OutputPath:        outputPath,
			RefName:           refName,
			Platform:          platform,
			ManifestBytes:     manifestBytes,
			ManifestDigest:    manifestDigest,
			ManifestMediaType: mediaType,
			ConfigDigest:      manifest.Config.Digest,
			ConfigBytes:       configBytes,
			Layers:            layerSources,
			TempDir:           filepath.Join(cacheDir, "tmp"),
			Progress:          packProgress,
		})
	}
	packProgress.Stop()
	if err != nil {
		return fmt.Errorf("生成镜像文件失败: %w", err)
	}

	if !opts.keepCache {
		_ = os.RemoveAll(cacheDir)
	} else {
		u.Infof("缓存已保留: %s", cacheDir)
	}

	u.Printf("")
	u.Printf("完成: %s (%s)", refName, platform.String())
	u.Printf("输出: %s", outputPath)
	if fi, err := os.Stat(outputPath); err == nil && !fi.IsDir() {
		u.Printf("大小: %s", ui.FormatBytes(fi.Size()))
	}
	if opts.format == "docker" || opts.format == "docker-legacy" {
		u.Printf("导入命令: docker load -i \"%s\"", outputPath)
	} else if strings.HasSuffix(strings.ToLower(outputPath), ".tar") {
		u.Printf("导入命令: skopeo copy oci-archive:\"%s\" docker-daemon:\"%s\"", outputPath, refName)
	} else {
		u.Printf("导入命令: skopeo copy oci:\"%s\" docker-daemon:\"%s\"", outputPath, refName)
	}
	return nil
}

func resolveCredentials(opts cliOptions, ref *reference.Reference, u *ui.UI) (registry.Credentials, error) {
	if opts.passwordStdin {
		line, err := readAllStdin()
		if err != nil {
			return registry.Credentials{}, fmt.Errorf("读取标准输入失败: %w", err)
		}
		opts.password = strings.TrimRight(line, "\r\n")
	}

	switch {
	case opts.username != "" && opts.password == "":
		return registry.Credentials{}, fmt.Errorf("已指定用户名但未提供密码（可使用 -password 或 -password-stdin）")
	case opts.username == "" && opts.password != "":
		return registry.Credentials{}, fmt.Errorf("已指定密码但未提供用户名")
	case opts.username != "":
		return registry.Credentials{Username: opts.username, Password: opts.password}, nil
	}

	if envUser := strings.TrimSpace(os.Getenv("DOCKER_USERNAME")); envUser != "" {
		return registry.Credentials{Username: envUser, Password: os.Getenv("DOCKER_PASSWORD")}, nil
	}

	c, err := registry.LoadDockerCredentials(ref.Registry)
	if err != nil {
		u.Verbosef("读取 Docker 凭据失败: %v", err)
	}
	if c.Empty() && ref.OriginalRegistry != ref.Registry {
		c, _ = registry.LoadDockerCredentials(ref.OriginalRegistry)
	}
	if !c.Empty() {
		u.Verbosef("使用 Docker 配置文件中的凭据，用户: %s", c.Username)
	}
	return c, nil
}

func totalLayerSize(layers []registry.Descriptor) int64 {
	var n int64
	for _, l := range layers {
		n += l.Size
	}
	return n
}

// hasZstdLayer 判断是否包含 zstd 压缩层。
func hasZstdLayer(layers []registry.Descriptor) bool {
	for _, l := range layers {
		if strings.Contains(l.MediaType, "zstd") {
			return true
		}
	}
	return false
}

func resolveOutput(out, format, base string) (string, error) {
	if out == "" {
		if format == "oci" {
			return base, nil
		}
		return base + ".tar", nil
	}
	if strings.HasSuffix(strings.ToLower(out), ".tar") {
		return out, nil
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	if format == "oci" {
		return filepath.Join(out, base), nil
	}
	return filepath.Join(out, base+".tar"), nil
}

func defaultCacheDir(outputPath string) string {
	dir := filepath.Dir(outputPath)
	base := strings.TrimSuffix(filepath.Base(outputPath), filepath.Ext(outputPath))
	if base == "" || base == "." {
		base = "image"
	}
	return filepath.Join(dir, "."+base+".cache")
}

func safeBaseName(ref *reference.Reference, platform registry.Platform) string {
	s := ref.Repository + "_" + ref.TagName() + "_" + platform.String()
	return sanitizeFileName(s)
}

func sanitizeFileName(s string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "@", "_", " ", "_",
	)
	s = replacer.Replace(s)
	s = strings.TrimRight(s, ". ")
	if s == "" {
		return "image"
	}
	return s
}

func resolvePlatform(opts cliOptions) (registry.Platform, bool, error) {
	explicit := opts.platform != "" || opts.osName != "" || opts.arch != "" || opts.variant != ""
	if opts.platform != "" {
		p, err := registry.ParsePlatform(opts.platform)
		if err != nil {
			return registry.Platform{}, true, err
		}
		if opts.arch != "" || opts.osName != "" || opts.variant != "" {
			if opts.osName != "" {
				p.OS = strings.ToLower(strings.TrimSpace(opts.osName))
			}
			if opts.arch != "" {
				p.Architecture = registry.NormalizeArch(opts.arch)
			}
			p.Variant = registry.NormalizeVariant(p.Architecture, opts.variant)
		}
		return p, explicit, nil
	}
	p := registry.DefaultPlatform()
	if opts.osName != "" {
		p.OS = strings.ToLower(strings.TrimSpace(opts.osName))
	}
	if opts.arch != "" {
		p.Architecture = registry.NormalizeArch(opts.arch)
	}
	p.Variant = registry.NormalizeVariant(p.Architecture, opts.variant)
	return p, explicit, nil
}

func printPlatforms(u *ui.UI, manifests []registry.Descriptor) {
	u.Printf("可用平台:")
	for _, d := range manifests {
		if d.Platform == nil || !d.Platform.IsReal() {
			continue
		}
		u.Printf("  %-22s %10s  %s", d.Platform.String(), ui.FormatBytes(d.Size), d.Digest)
	}
}

func promptPlatform(u *ui.UI, available []string, want registry.Platform) (registry.Platform, error) {
	if len(available) == 0 {
		return registry.Platform{}, fmt.Errorf("该镜像没有可用平台")
	}
	u.Printf("当前平台 %s 不可用，可用平台: %s", want.String(), strings.Join(available, ", "))
	for i := 0; i < 3; i++ {
		line, ok := promptLine(fmt.Sprintf("请输入平台（默认 %s）: ", available[0]))
		if !ok {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = available[0]
		}
		p, err := registry.ParsePlatform(line)
		if err != nil {
			u.Printf("无效的平台: %v", err)
			continue
		}
		for _, a := range available {
			ap, err := registry.ParsePlatform(a)
			if err == nil && registry.Match(p, ap) {
				return ap, nil
			}
		}
		u.Printf("平台 %s 不在可用列表中", p.String())
	}
	return registry.Platform{}, fmt.Errorf("未选择有效平台")
}

var stdinReader *bufio.Reader

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func promptLine(prompt string) (string, bool) {
	if stdinReader == nil {
		stdinReader = bufio.NewReader(os.Stdin)
	}
	fmt.Print(prompt)
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return line, true
}

func readAllStdin() (string, error) {
	if stdinReader == nil {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			line, ok := promptLine("请输入密码: ")
			if !ok {
				return "", fmt.Errorf("未读取到密码")
			}
			return line, nil
		}
		stdinReader = bufio.NewReader(os.Stdin)
	}
	data, err := io.ReadAll(stdinReader)
	return string(data), err
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	mult := float64(1)
	units := []struct {
		suffix string
		mul    float64
	}{
		{"KIB", 1 << 10}, {"KB", 1 << 10}, {"K", 1 << 10},
		{"MIB", 1 << 20}, {"MB", 1 << 20}, {"M", 1 << 20},
		{"GIB", 1 << 30}, {"GB", 1 << 30}, {"G", 1 << 30},
		{"TIB", 1 << 40}, {"TB", 1 << 40}, {"T", 1 << 40},
		{"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			mult = u.mul
			upper = strings.TrimSpace(strings.TrimSuffix(upper, u.suffix))
			break
		}
	}
	v, err := strconv.ParseFloat(upper, 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小 %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("大小不能为负数: %q", s)
	}
	return int64(v * mult), nil
}
