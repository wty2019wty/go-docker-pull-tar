# go-docker-pull-tar

一个用 **纯 Go** 重写的 Docker 镜像拉取工具：不依赖 Docker、不依赖 Python、不启动守护进程，直接调用 Docker Registry HTTP API V2 把镜像下载并打包成 `docker load` 可导入的 tar 包，或 OCI 镜像布局。

没有 Web UI、没有搜索、没有多余功能，只做一件事——**把镜像完整、快速、可断点续传地拉下来**。

## 特性

- **纯 Go，零第三方依赖**：只使用标准库，`go build` 即可得到单文件可执行程序。
- **多线程下载**：所有层的分片进入统一工作池，`-workers` 控制并发连接数。
- **分片 + Range**：大层自动切成固定大小分片并行下载；服务器不支持 Range 时自动退化为单流。
- **断点续传**：分片文件与校验状态保存在缓存目录，中断后重新运行相同命令即可继续。
- **登录认证**：
  - `-u/-p` 显式指定用户名密码；
  - `-password-stdin` 从标准输入读取密码；
  - 自动读取 `DOCKER_USERNAME` / `DOCKER_PASSWORD`；
  - 自动读取 Docker CLI 的 `~/.docker/config.json`（支持 `DOCKER_CONFIG`）。
- **代理**：支持 `http://`、`https://`、`socks5://`、`socks5h://`，未指定时自动读取系统代理环境变量。
- **指定架构**：支持多平台索引，`-platform linux/arm64`、`-arch arm64`、`-os linux -variant v7` 均可；`-list-platforms` 查看全部平台。
- **镜像站**：`-registry docker.1ms.run` 可将请求切换到镜像站，同时保持原镜像名用于打标签。
- **完整性校验**：每个 Blob 做 SHA256 校验，docker 格式额外校验配置中的 `diff_ids`。
- **两种输出**：
  - `docker`（默认）：与 Docker 27+ 官方 `docker save` 一致的混合格式，`docker load -i xxx.tar` 直接导入；
  - `oci`：标准 OCI image layout 目录或 `.tar`；
  - `docker-legacy`：旧版未压缩 `layer.tar` 结构，兼容老版本 Docker。
- **TLS 选项**：`-insecure` 跳过证书校验，`-plain-http` 访问本地 HTTP 仓库。

## 快速开始

要求 Go 1.22 及以上。

```bash
cd go-docker-pull-tar
go build -trimpath -ldflags "-s -w" -o gdockerpull
```

Windows 下会得到 `gdockerpull.exe`，也可以直接运行：

```bat
build.bat
```

### 拉取第一个镜像

```bash
# 默认 linux/<当前主机架构>，输出 ./library_nginx_latest_linux_amd64.tar
gdockerpull nginx:latest

# 指定架构
gdockerpull -i alpine:3.20 -a arm64

# 指定平台完整写法
gdockerpull -i alpine:3.20 -platform linux/arm/v7

# 私有仓库登录
gdockerpull -i harbor.example.com/team/app:v1 -u admin -p secret

# 使用镜像站 + SOCKS5 代理 + 16 并发
gdockerpull -i nginx:1.25 -registry docker.1ms.run \
  -proxy socks5://127.0.0.1:1080 -workers 16

# 只查看支持的平台
gdockerpull -i nginx:latest -list-platforms

# 输出 OCI 布局
gdockerpull -i nginx:latest -format oci -o ./oci-out
```

导入镜像：

```bash
docker load -i ./library_nginx_latest_linux_amd64.tar
```

## 参数说明

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-i, -image` | 镜像名称，也可直接作为位置参数 | 必填 |
| `-platform` | 目标平台，如 `linux/amd64`、`linux/arm/v7` | `linux/<主机架构>` |
| `-os` | 目标操作系统 | `linux` |
| `-arch, -a` | 目标架构，如 `amd64`、`arm64` | 主机架构 |
| `-variant` | 架构变体，如 `v7` | 空 |
| `-list-platforms` | 列出多平台索引中的平台后退出 | `false` |
| `-u, -username` | 仓库用户名 | 空 |
| `-p, -password` | 仓库密码 | 空 |
| `-password-stdin` | 从标准输入读取密码 | `false` |
| `-tag-name` | 写入 tar 包的镜像名 | 自动生成 |
| `-registry, -r` | 镜像站地址，如 `docker.1ms.run` | 空 |
| `-proxy` | 代理地址，支持 http/https/socks5 | 环境变量 |
| `-insecure` | 跳过 TLS 证书校验 | `false` |
| `-plain-http` | 使用 HTTP 访问仓库 | `false` |
| `-workers, -w` | 并发下载连接数 | `8` |
| `-chunk-size` | 大层分片大小，`0` 表示禁用分片 | `8MiB` |
| `-min-chunk-size` | 启用分片的最小层大小 | `16MiB` |
| `-retries` | 单个请求最大重试次数 | `8` |
| `-format` | 输出格式：`docker`、`docker-legacy` 或 `oci` | `docker` |
| `-o, -output` | 输出文件或目录 | 自动生成 |
| `-cache-dir` | 断点续传缓存目录 | 输出文件旁的隐藏目录 |
| `-keep-cache` | 成功后保留缓存 | `false` |
| `-q, -quiet` | 静默模式 | `false` |
| `-verbose` | 输出详细调试日志 | `false` |
| `-version, -V` | 显示版本 | - |

> 提示：参数需要写在镜像名之前，例如 `gdockerpull -a arm64 nginx:latest`。

## 平台与架构

- 默认平台是 **Linux + 当前主机架构**，因为在 Windows/macOS 上拉取 Docker 镜像时，目标几乎总是 Linux。
- 如果镜像是多平台索引，工具会按 `-platform/-os/-arch/-variant` 选择对应清单。
- 如果默认平台不存在且当前是交互式终端，会提示从可用平台中选择；非交互环境则直接报错并列出可用平台。
- `linux/arm64/v8` 这类带变体的平台也支持；`-arch armv7`、`-arch aarch64` 等别名会自动转换。

```bash
gdockerpull -i nginx:latest -list-platforms
gdockerpull -i nginx:latest -platform linux/arm64
gdockerpull -i nginx:latest -os linux -arch arm -variant v7
```

## 登录认证

按以下优先级选择凭据：

1. `-u/-p` 或 `-password-stdin`；
2. `DOCKER_USERNAME` / `DOCKER_PASSWORD` 环境变量；
3. Docker CLI 配置文件 `~/.docker/config.json` 中的 `auths`（可用 `DOCKER_CONFIG` 指定目录）。

```bash
# 命令行
gdockerpull -i registry.example.com/app:1.0 -u admin -p 'p@ssw0rd'

# 从标准输入（避免密码进入命令历史）
echo 'p@ssw0rd' | gdockerpull -i registry.example.com/app:1.0 -u admin -password-stdin

# 使用已登录的 Docker 配置
docker login registry.example.com
gdockerpull -i registry.example.com/app:1.0
```

支持的认证方式：

- `WWW-Authenticate: Bearer ...`：自动向 `realm` 换取 Token（Docker Hub、GHCR、Quay、Harbor 等）。
- `WWW-Authenticate: Basic ...`：直接使用 Basic 认证。

> 暂不支持 `credsStore` / `credHelpers`（需要外部 credential helper 程序）。这类配置请改用 `-u/-p` 或环境变量。

## 代理

```bash
# HTTP/HTTPS 代理
gdockerpull -i nginx:latest -proxy http://127.0.0.1:7890

# 带认证的代理
gdockerpull -i nginx:latest -proxy http://user:pass@127.0.0.1:7890

# SOCKS5
gdockerpull -i nginx:latest -proxy socks5://127.0.0.1:1080

# 带账号密码的 SOCKS5
gdockerpull -i nginx:latest -proxy socks5://user:pass@127.0.0.1:1080
```

未传 `-proxy` 时，自动使用 `HTTPS_PROXY` / `HTTP_PROXY`（以及小写形式）环境变量。

## 镜像站

```bash
gdockerpull -i nginx:latest -registry docker.1ms.run
gdockerpull -i nginx:latest -r docker.1ms.run
```

镜像站只影响实际请求地址，`docker load` 后的镜像名仍然是 `nginx:latest`，不会带上镜像站域名。

## 输出格式

### docker（默认）

与 Docker 27+ 官方 `docker save` 一致的混合格式，tar 内部包含：

```text
blobs/sha256/<config-digest>
blobs/sha256/<layer-digest>
blobs/sha256/<manifest-digest>
oci-layout
index.json
manifest.json
repositories
```

- 层保持原始压缩格式（gzip/zstd），不需要解压，速度快、临时磁盘占用小。
- `docker load` 会读取 `manifest.json`，并按配置中的 `diff_ids` 校验层。
- 同时包含 `index.json`/`oci-layout`，因此这个 tar 也可以被 OCI 工具识别。
- 官方 `docker load`（20.10、24、27 的源码均确认）通过 `manifest.json` 读取层，并使用 `archive.DecompressStream` 自动识别 gzip，因此压缩层可以直接导入。

```bash
gdockerpull -i nginx:latest
docker load -i library_nginx_latest_linux_amd64.tar
```

### docker-legacy

与旧版 `docker save` 相同的经典结构：每层解压成未压缩的 `layer.tar`。

```bash
gdockerpull -i nginx:latest -format docker-legacy
```

```text
<config>.json
<layer-id>/layer.tar
<layer-id>/json
<layer-id>/VERSION
manifest.json
repositories
```

适合需要兼容很老的 Docker 或需要直接查看未压缩层内容的场景。该格式遇到 zstd 层会报错。

### OCI

```bash
# 默认输出目录
gdockerpull -i nginx:latest -format oci

# 指定目录容器（会在目录下创建布局）
gdockerpull -i nginx:latest -format oci -o ./out

# 直接输出 tar 包
gdockerpull -i nginx:latest -format oci -o ./nginx-oci.tar
```

OCI 布局只包含标准文件，适合 `skopeo`、`containerd` 等工具：

```bash
skopeo copy oci:./library_nginx_latest_linux_amd64 docker-daemon:nginx:latest
skopeo copy oci-archive:./nginx-oci.tar docker-daemon:nginx:latest
```

## 断点续传与缓存

默认缓存目录位于输出文件旁边，例如：

```text
.library_nginx_latest_linux_amd64.cache/
├── state.json          # 清单摘要、平台、已校验对象
├── objects/            # 已下载并校验的压缩 Blob
├── parts/              # 未完成的分片
└── tmp/                # 打包临时文件
```

- 下载中断后，重新运行**相同命令**即可继续。
- 清单摘要变化（例如 tag 指向了新镜像）时会自动清空旧缓存。
- 修改 `-chunk-size` 会使旧分片失效并重新下载。
- 成功后默认删除缓存；加 `-keep-cache` 可保留。
- 缓存目录可用 `-cache-dir` 指定。

```bash
# 中断后继续
gdockerpull -i ubuntu:24.04

# 保留缓存便于观察或二次打包
gdockerpull -i ubuntu:24.04 -keep-cache
```

## 性能建议

- `-workers 8` 适合大多数网络；大带宽环境可提高到 `16` 或 `32`。
- 默认 `8MiB` 分片适合几十 MB 到几 GB 的镜像；如果单层特别大，可适当增大 `-chunk-size`。
- 若镜像站/CDN 对 Range 支持不好，工具会自动退化为单流；也可以显式 `-chunk-size 0` 关闭分片。
- 并发过高可能触发仓库限流（HTTP 429），工具会自动指数退避重试。

## 实现说明

本工具依据 Docker Registry HTTP API V2 与 OCI 镜像规范实现：

- 认证：解析 `WWW-Authenticate`，向 `realm` 请求 `scope=repository:<name>:pull` 的 Token。
- 清单：请求
  `application/vnd.docker.distribution.manifest.v2+json`、
  `application/vnd.docker.distribution.manifest.list.v2+json`、
  `application/vnd.oci.image.manifest.v1+json`、
  `application/vnd.oci.image.index.v1+json`。
- 多平台：从 index 中按 `os/architecture/variant` 选择子清单，并校验子清单摘要。
- Blob：`GET /v2/<name>/blobs/<digest>`，使用 `Range` 分片；跨域重定向时由 Go 自动剥离 `Authorization`。
- 校验：下载完成后对整个 Blob 计算 SHA256；docker 打包时再校验 `rootfs.diff_ids`。
- 打包：默认按 Docker 27+ `docker save` 的混合格式生成 `blobs/sha256/*` + `manifest.json` + `oci-layout` + `index.json`；`docker-legacy` 生成旧版未压缩 `layer.tar`；OCI 格式生成标准 `oci-layout`、`index.json` 与 `blobs/sha256/*`。

## 项目结构

```text
go-docker-pull-tar/
├── main.go                     # CLI、流程编排
├── internal/
│   ├── reference/              # 镜像引用解析
│   ├── registry/               # Registry API V2 客户端、认证、平台选择
│   ├── download/               # 多线程分片下载、断点续传、校验
│   ├── transport/              # HTTP 客户端、TLS、HTTP/SOCKS5 代理
│   ├── save/                   # docker save tar（新/旧格式）、OCI 布局
│   └── ui/                     # 终端输出与进度
├── go.mod
├── build.bat
└── README.md
```

## 常见问题

**Q：提示“不支持平台”，怎么办？**
A：先用 `-list-platforms` 查看镜像实际支持的平台，再用 `-platform` 或 `-arch` 指定。注意默认是 `linux/<主机架构>`。

**Q：拉取私有仓库返回 401？**
A：确认用户名密码正确；Harbor 等仓库可能需要先在 Web 端创建机器人账号。可用 `-verbose` 查看认证请求。也可以先 `docker login`，让工具读取 Docker 配置。

**Q：在公司代理后面无法连接？**
A：显式传 `-proxy`，或设置 `HTTPS_PROXY`。SOCKS5 请使用 `socks5://` 或 `socks5h://`。

**Q：证书报错？**
A：自签名/内部仓库可加 `-insecure`；本地 HTTP 仓库加 `-plain-http`。

**Q：镜像层是 zstd 压缩？**
A：默认的 `docker` 格式会原样保留压缩层，需要 Docker 27+ 才能导入；旧版 Docker 请使用 `-format oci`。`docker-legacy` 需要解压，遇到 zstd 会报错。

**Q：按 digest 拉取后镜像名是什么？**
A：默认使用 `仓库:latest` 作为 `RepoTags`，可用 `-tag-name repo:tag` 覆盖。

## 许可证

MIT License，详见 [LICENSE](LICENSE)。
