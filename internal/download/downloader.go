// Package download 实现多线程、可断点续传的 Blob 下载。
package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wty2019wty/go-docker-pull-tar/internal/registry"
	"github.com/wty2019wty/go-docker-pull-tar/internal/ui"
)

// Blob 描述一个需要下载的对象。
type Blob struct {
	Digest    string
	Size      int64
	URLs      []string
	MediaType string
}

// errRangeUnsupported 表示服务器忽略了 Range 请求。
var errRangeUnsupported = errors.New("服务器不支持 Range 请求")

// Options 是下载器参数。
type Options struct {
	Client         *registry.Client
	HTTPClient     *http.Client
	CacheDir       string
	ManifestDigest string
	Platform       string
	Workers        int
	ChunkSize      int64
	MinChunkSize   int64
	// Retries 是单个请求的最大重试次数，0 表示不重试。
	Retries  int
	Progress *ui.Progress
	UI       *ui.UI
}

// Downloader 负责把一个镜像的所有层下载到缓存目录。
type Downloader struct {
	opts Options

	state   state
	stateMu sync.Mutex

	blobs []*blobState
}

type state struct {
	ManifestDigest string           `json:"manifest_digest"`
	Platform       string           `json:"platform"`
	ChunkSize      int64            `json:"chunk_size"`
	Verified       map[string]int64 `json:"verified"`
}

type blobState struct {
	blob        *Blob
	progressIdx int
	verified    bool
	chunked     bool
	parts       []*part
}

type part struct {
	start int64
	end   int64 // 闭区间；-1 表示到文件末尾
	path  string
}

type task struct {
	blob *blobState
	part *part
}

// New 创建下载器。
func New(opts Options) (*Downloader, error) {
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.ChunkSize < 0 {
		opts.ChunkSize = 0
	}
	if opts.MinChunkSize < 0 {
		opts.MinChunkSize = 0
	}
	if opts.Progress == nil {
		opts.Progress = ui.NewProgress(opts.UI)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	d := &Downloader{opts: opts}
	for _, dir := range []string{"", "objects", "parts", "tmp"} {
		if err := os.MkdirAll(filepath.Join(opts.CacheDir, dir), 0o700); err != nil {
			return nil, err
		}
	}
	if err := d.loadState(); err != nil {
		return nil, err
	}
	return d, nil
}

// HexDigest 去掉算法前缀，返回摘要的十六进制部分。
func HexDigest(digest string) (string, error) {
	i := strings.IndexByte(digest, ':')
	if i <= 0 {
		return "", fmt.Errorf("无效的摘要: %q", digest)
	}
	algo := digest[:i]
	if algo != "sha256" {
		return "", fmt.Errorf("暂不支持 %s 摘要，仅支持 sha256", algo)
	}
	hexPart := digest[i+1:]
	if len(hexPart) != 64 {
		return "", fmt.Errorf("无效的 sha256 摘要: %q", digest)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return "", fmt.Errorf("无效的 sha256 摘要: %q", digest)
	}
	return strings.ToLower(hexPart), nil
}

// ObjectPath 返回 Blob 在缓存中的最终路径。
func ObjectPath(cacheDir, digest string) (string, error) {
	h, err := HexDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "objects", h+".blob"), nil
}

func (d *Downloader) loadState() error {
	d.state = state{
		ManifestDigest: d.opts.ManifestDigest,
		Platform:       d.opts.Platform,
		ChunkSize:      d.opts.ChunkSize,
		Verified:       make(map[string]int64),
	}
	path := filepath.Join(d.opts.CacheDir, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		// 状态文件损坏时直接重建。
		return nil
	}
	if s.Verified == nil {
		s.Verified = make(map[string]int64)
	}
	if s.ManifestDigest != "" && s.ManifestDigest != d.opts.ManifestDigest {
		// 镜像清单已经变化，清空旧缓存。
		d.wipe()
		return nil
	}
	if s.ChunkSize != 0 && s.ChunkSize != d.opts.ChunkSize {
		// 分片大小变化后旧分片无法复用。
		_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "parts"))
		_ = os.MkdirAll(filepath.Join(d.opts.CacheDir, "parts"), 0o755)
		s.Verified = make(map[string]int64)
	}
	d.state = s
	return nil
}

func (d *Downloader) wipe() {
	_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "parts"))
	_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "objects"))
	_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "tmp"))
	for _, dir := range []string{"", "objects", "parts", "tmp"} {
		_ = os.MkdirAll(filepath.Join(d.opts.CacheDir, dir), 0o700)
	}
}

func (d *Downloader) saveState() error {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	d.state.ManifestDigest = d.opts.ManifestDigest
	d.state.Platform = d.opts.Platform
	d.state.ChunkSize = d.opts.ChunkSize
	data, err := json.MarshalIndent(d.state, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(d.opts.CacheDir, "state.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Download 下载全部层，完成校验后返回。
func (d *Downloader) Download(ctx context.Context, blobs []Blob) error {
	if len(blobs) == 0 {
		return nil
	}

	// 1. 检查已有对象，构建计划。
	seen := make(map[string]bool, len(blobs))
	for i := range blobs {
		b := blobs[i]
		if _, err := HexDigest(b.Digest); err != nil {
			return err
		}
		if seen[b.Digest] {
			continue
		}
		seen[b.Digest] = true
		st := &blobState{blob: &b}
		d.blobs = append(d.blobs, st)

		objectPath, err := ObjectPath(d.opts.CacheDir, b.Digest)
		if err != nil {
			return err
		}

		st.progressIdx = d.opts.Progress.AddLayer(shortDigest(b.Digest), b.Size)

		if fi, err := os.Stat(objectPath); err == nil && fi.Mode().IsRegular() {
			// 状态文件记录了该对象已校验且大小一致时，直接复用，避免重复读盘。
			if recorded := d.state.Verified[b.Digest]; recorded > 0 && recorded == fi.Size() {
				st.verified = true
				d.opts.Progress.SetLayerTotal(st.progressIdx, fi.Size())
				d.opts.Progress.MarkLayerDone(st.progressIdx)
				continue
			}
			// 否则重新计算摘要确认完整性，并把读取的字节计入进度条。
			d.opts.UI.Infof("正在校验缓存层 %s（%s）...", shortDigest(b.Digest), ui.FormatBytes(fi.Size()))
			var fed int64
			idx := st.progressIdx
			valid, err := d.verifyObject(objectPath, b.Digest, func(n int64) {
				fed += n
				d.opts.Progress.Add(idx, n)
			})
			if err == nil && valid {
				st.verified = true
				d.state.Verified[b.Digest] = fi.Size()
				d.opts.Progress.SetLayerTotal(st.progressIdx, fi.Size())
				d.opts.Progress.MarkLayerDone(st.progressIdx)
				d.opts.UI.Infof("层 %s 校验通过（%s）", shortDigest(b.Digest), ui.FormatBytes(fi.Size()))
				continue
			}
			if fed > 0 {
				// 校验失败时回退记账，避免进度虚高。
				d.opts.Progress.Add(idx, -fed)
			}
			_ = os.Remove(objectPath)
			delete(d.state.Verified, b.Digest)
		}
	}

	// 2. 探测支持 Range 的大层，并生成分片计划。
	if err := d.plan(ctx); err != nil {
		return err
	}

	// 3. 下载 + 校验；校验失败自动重下一次，中途 Range 失效则退化为单流后重下。
	var lastErr error
	verifyRetries := 0
	fallbackSingle := false
	for {
		tasks := d.buildTasks()
		if len(tasks) > 0 {
			if err := d.runTasks(ctx, tasks); err != nil {
				if errors.Is(err, errRangeUnsupported) && !fallbackSingle {
					// 服务器中途不再支持 Range：丢弃分片计划，整体退化为单流重新下载。
					fallbackSingle = true
					d.opts.UI.Printf("警告: 服务器不再支持 Range 请求，自动退化为单流下载")
					d.disableChunking()
					continue
				}
				return err
			}
			d.opts.UI.Infof("分片下载完成，正在合并并校验层数据...")
		}

		bad, err := d.assembleAndVerify()
		if err != nil {
			return err
		}
		if len(bad) == 0 {
			return d.saveState()
		}

		lastErr = fmt.Errorf("%d 个层校验失败", len(bad))
		verifyRetries++
		if verifyRetries > 1 {
			return lastErr
		}
		for _, st := range bad {
			d.opts.UI.Printf("警告: 层 %s 校验失败，重新下载", shortDigest(st.blob.Digest))
			d.resetBlob(st)
		}
	}
}

func shortDigest(digest string) string {
	h, err := HexDigest(digest)
	if err != nil {
		if len(digest) > 12 {
			return digest[:12]
		}
		return digest
	}
	return h[:12]
}

// plan 探测 Range 支持并生成分片，同时统计已完成分片。
func (d *Downloader) plan(ctx context.Context) error {
	type probeResult struct {
		size      int64
		supported bool
	}
	results := make([]probeResult, len(d.blobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, d.opts.Workers)

	for i, st := range d.blobs {
		if st.verified {
			continue
		}
		size := st.blob.Size
		needProbe := size <= 0 || (d.opts.ChunkSize > 0 && size >= d.opts.MinChunkSize)
		if !needProbe {
			continue
		}
		wg.Add(1)
		go func(i int, st *blobState) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			size := st.blob.Size
			if size <= 0 {
				size = d.opts.Client.HeadBlob(ctx, st.blob.Digest)
			}
			// 只有可能需要分片时才探测 Range，避免多余请求。
			if d.opts.ChunkSize > 0 {
				total, ok := d.opts.Client.ProbeRange(ctx, st.blob.Digest)
				if total > 0 {
					size = total
				}
				results[i] = probeResult{size: size, supported: ok}
				return
			}
			results[i] = probeResult{size: size}
		}(i, st)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	for i, st := range d.blobs {
		if st.verified {
			continue
		}
		if results[i].size > 0 {
			st.blob.Size = results[i].size
			d.opts.Progress.SetLayerTotal(st.progressIdx, st.blob.Size)
		}
		// 生成分片计划。
		st.parts = d.makeParts(st.blob, results[i].supported)
		st.chunked = len(st.parts) > 1
		if st.blob.Size > 0 {
			d.opts.Progress.SetLayerTotal(st.progressIdx, st.blob.Size)
		}

		var doneBytes int64
		doneParts := 0
		for _, p := range st.parts {
			if size, ok := partComplete(p); ok {
				doneBytes += size
				doneParts++
				continue
			}
			// 未完成分片中已写入的字节同样计入完成量，保证续传记账精确。
			if fi, err := os.Stat(p.path); err == nil && fi.Mode().IsRegular() {
				doneBytes += fi.Size()
			}
		}
		d.opts.Progress.SetLayerPlan(st.progressIdx, len(st.parts), doneParts, doneBytes)
	}
	return d.saveState()
}

func (d *Downloader) makeParts(blob *Blob, rangeSupported bool) []*part {
	hexDigest, err := HexDigest(blob.Digest)
	if err != nil {
		return nil
	}
	dir := filepath.Join(d.opts.CacheDir, "parts", hexDigest)
	_ = os.MkdirAll(dir, 0o700)

	if blob.Size <= 0 {
		return []*part{{start: 0, end: -1, path: filepath.Join(dir, "single")}}
	}

	if d.opts.ChunkSize <= 0 || !rangeSupported || blob.Size < d.opts.MinChunkSize {
		return []*part{{start: 0, end: blob.Size - 1, path: filepath.Join(dir, "single")}}
	}

	count := (blob.Size + d.opts.ChunkSize - 1) / d.opts.ChunkSize
	if count <= 1 {
		return []*part{{start: 0, end: blob.Size - 1, path: filepath.Join(dir, "single")}}
	}

	parts := make([]*part, 0, count)
	for i := int64(0); i < count; i++ {
		start := i * d.opts.ChunkSize
		end := start + d.opts.ChunkSize - 1
		if end >= blob.Size {
			end = blob.Size - 1
		}
		parts = append(parts, &part{
			start: start,
			end:   end,
			path:  filepath.Join(dir, fmt.Sprintf("%d-%d", start, end)),
		})
	}
	return parts
}

func partComplete(p *part) (int64, bool) {
	fi, err := os.Stat(p.path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	if p.end < 0 {
		// 大小未知的流式下载依靠 .done 标记判断完整性。
		if _, err := os.Stat(p.path + ".done"); err != nil {
			return 0, false
		}
		return fi.Size(), true
	}
	expected := p.end - p.start + 1
	if fi.Size() == expected {
		return fi.Size(), true
	}
	return 0, false
}

func (d *Downloader) buildTasks() []task {
	var tasks []task
	for _, st := range d.blobs {
		if st.verified {
			continue
		}
		for _, p := range st.parts {
			if _, ok := partComplete(p); ok {
				continue
			}
			tasks = append(tasks, task{blob: st, part: p})
		}
	}
	return tasks
}

func (d *Downloader) runTasks(ctx context.Context, tasks []task) error {
	if len(tasks) == 0 {
		return nil
	}
	workers := d.opts.Workers
	if workers > len(tasks) {
		workers = len(tasks)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan task)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if runCtx.Err() != nil {
					return
				}
				if err := d.downloadPart(runCtx, t); err != nil {
					setErr(err)
					return
				}
				d.opts.Progress.PartDone(t.blob.progressIdx)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, t := range tasks {
			select {
			case jobs <- t:
			case <-runCtx.Done():
				return
			}
		}
	}()

	wg.Wait()
	errMu.Lock()
	err := firstErr
	errMu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (d *Downloader) downloadPart(ctx context.Context, t task) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := d.tryPart(ctx, t)
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isFatal(err) {
			return err
		}
		if attempt >= d.opts.Retries {
			break
		}
		wait := time.Duration(1<<min(attempt, 5)) * 500 * time.Millisecond
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		d.opts.UI.Printf("层 %s 分片 %d-%d 下载失败: %v（%s 后重试 %d/%d）",
			shortDigest(t.blob.blob.Digest), t.part.start, t.part.end, err, wait, attempt+1, d.opts.Retries)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("层 %s 分片下载失败: %w", shortDigest(t.blob.blob.Digest), lastErr)
}

func isFatal(err error) bool {
	return errors.Is(err, errRangeUnsupported)
}

func (d *Downloader) tryPart(ctx context.Context, t task) error {
	p := t.part
	blob := t.blob.blob

	var offset int64
	if fi, err := os.Stat(p.path); err == nil && fi.Mode().IsRegular() {
		offset = fi.Size()
		expected := p.end - p.start + 1
		if p.end >= 0 && offset > expected {
			d.opts.Progress.Add(t.blob.progressIdx, -offset)
			if err := os.Remove(p.path); err != nil {
				return err
			}
			offset = 0
		}
		if p.end >= 0 && offset == expected {
			return nil
		}
	}

	start := p.start + offset
	if p.end >= 0 && start > p.end {
		return nil
	}
	// 单分片且从头下载时不必发送 Range，兼容不支持 Range 的服务器。
	rangeStart := start
	if offset == 0 && !t.blob.chunked {
		rangeStart = -1
	}

	// 每次读取设置空闲超时，避免连接卡死。
	idleCtx, idleCancel := context.WithCancel(ctx)
	defer idleCancel()
	var idleFired bool
	var idleMu sync.Mutex
	timer := time.AfterFunc(5*time.Minute, func() {
		idleMu.Lock()
		idleFired = true
		idleMu.Unlock()
		idleCancel()
	})
	defer timer.Stop()

	resp, err := d.openPart(idleCtx, blob, rangeStart, p.end)
	if err != nil {
		idleMu.Lock()
		fired := idleFired
		idleMu.Unlock()
		if fired {
			return fmt.Errorf("读取超时（5 分钟无数据）")
		}
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if cs, ce, ok := parseContentRange(resp.Header.Get("Content-Range")); ok {
			if cs != start {
				return fmt.Errorf("服务器返回的 Range 起点不匹配: 期望 %d，实际 %d", start, cs)
			}
			if p.end >= 0 && ce != p.end {
				// 服务器可能截断，仍按实际内容校验大小。
				_ = ce
			}
		}
	case http.StatusOK:
		if t.blob.chunked {
			// 分片请求收到整段响应，说明服务器忽略了 Range 头，退化为单流处理。
			return fmt.Errorf("%w（分片 %d-%d）", errRangeUnsupported, p.start, p.end)
		}
		if p.start > 0 {
			return fmt.Errorf("%w（分片起点 %d）", errRangeUnsupported, p.start)
		}
		if offset > 0 {
			// 服务器忽略了 Range，只能从头开始。
			d.opts.Progress.Add(t.blob.progressIdx, -offset)
			offset = 0
			start = p.start
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if p.end >= 0 {
			if fi, err := os.Stat(p.path); err == nil && fi.Size() == p.end-p.start+1 {
				return nil
			}
		} else if offset > 0 {
			// 大小未知且从文件末尾发起 Range 得到 416，说明已经下载完整。
			return writeDone(p)
		}
		return fmt.Errorf("服务器返回 416")
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(p.path, flags, 0o600)
	if err != nil {
		return err
	}
	if offset > 0 {
		// 断点续传必须定位到已有数据末尾，否则会从文件头覆盖写。
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
	}

	buf := make([]byte, 128<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			timer.Reset(5 * time.Minute)
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			d.opts.Progress.AddDownloaded(t.blob.progressIdx, int64(n))
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			f.Close()
			idleMu.Lock()
			fired := idleFired
			idleMu.Unlock()
			if fired {
				return fmt.Errorf("读取超时（5 分钟无数据）")
			}
			return readErr
		}
	}
	if err := f.Close(); err != nil {
		return err
	}

	if p.end >= 0 {
		expected := p.end - p.start + 1
		if fi, err := os.Stat(p.path); err != nil || fi.Size() != expected {
			return fmt.Errorf("分片大小不完整: 期望 %d", expected)
		}
	} else {
		if err := writeDone(p); err != nil {
			return err
		}
	}
	return nil
}

func writeDone(p *part) error {
	fi, err := os.Stat(p.path)
	if err != nil {
		return err
	}
	return os.WriteFile(p.path+".done", []byte(strconv.FormatInt(fi.Size(), 10)), 0o600)
}

// openPart 优先从 Registry 下载，失败时尝试 foreign layer 提供的外部地址。
func (d *Downloader) openPart(ctx context.Context, blob *Blob, start, end int64) (*http.Response, error) {
	resp, err := d.opts.Client.BlobRequest(ctx, blob.Digest, start, end)
	if err == nil {
		return resp, nil
	}
	if len(blob.URLs) == 0 {
		return nil, err
	}
	var lastErr error = err
	for _, raw := range blob.URLs {
		u := raw
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		} else if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			if base, e := url.Parse("https://" + d.opts.Client.Registry()); e == nil {
				if ref, e2 := url.Parse(u); e2 == nil {
					u = base.ResolveReference(ref).String()
				}
			}
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if e != nil {
			lastErr = e
			continue
		}
		req.Header.Set("Accept-Encoding", "identity")
		if start >= 0 {
			if end >= 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
			} else {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
			}
		}
		resp, e := d.opts.HTTPClient.Do(req)
		if e != nil {
			lastErr = e
			continue
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			return resp, nil
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		lastErr = fmt.Errorf("外部地址返回 HTTP %d", resp.StatusCode)
	}
	return nil, lastErr
}

func parseContentRange(s string) (start, end int64, ok bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, false
	}
	s = strings.TrimPrefix(s, "bytes ")
	slash := strings.IndexByte(s, '/')
	if slash >= 0 {
		s = s[:slash]
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	var e error
	start, e = parseInt64(parts[0])
	if e != nil {
		return 0, 0, false
	}
	end, e = parseInt64(parts[1])
	if e != nil {
		return 0, 0, false
	}
	return start, end, true
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid")
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

// assembleAndVerify 合并分片并校验摘要，返回校验失败的层。
func (d *Downloader) assembleAndVerify() ([]*blobState, error) {
	var bad []*blobState
	for _, st := range d.blobs {
		if st.verified {
			continue
		}
		objectPath, err := ObjectPath(d.opts.CacheDir, st.blob.Digest)
		if err != nil {
			return nil, err
		}

		// 所有分片必须齐全。
		allDone := true
		for _, p := range st.parts {
			if _, ok := partComplete(p); !ok {
				allDone = false
				break
			}
		}
		if !allDone {
			return nil, fmt.Errorf("层 %s 的分片不完整", shortDigest(st.blob.Digest))
		}

		d.opts.UI.Infof("正在合并并校验层 %s（%s）...", shortDigest(st.blob.Digest), ui.FormatBytes(st.blob.Size))
		if err := d.assemble(st, objectPath); err != nil {
			return nil, err
		}
		valid, err := d.verifyObject(objectPath, st.blob.Digest, nil)
		if err != nil {
			return nil, err
		}
		if !valid {
			bad = append(bad, st)
			continue
		}
		fi, err := os.Stat(objectPath)
		if err != nil {
			return nil, err
		}
		if st.blob.Size <= 0 {
			st.blob.Size = fi.Size()
			d.opts.Progress.SetLayerTotal(st.progressIdx, fi.Size())
		}
		st.verified = true
		d.state.Verified[st.blob.Digest] = fi.Size()
		d.opts.Progress.MarkLayerDone(st.progressIdx)
		d.opts.UI.Infof("层 %s 校验通过（%s）", shortDigest(st.blob.Digest), ui.FormatBytes(fi.Size()))
		_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "parts", mustHex(st.blob.Digest)))
	}
	return bad, nil
}

func (d *Downloader) assemble(st *blobState, objectPath string) error {
	if len(st.parts) == 0 {
		return fmt.Errorf("层 %s 没有分片计划", shortDigest(st.blob.Digest))
	}
	if len(st.parts) == 1 {
		// 单分片直接重命名，避免额外拷贝。
		p := st.parts[0]
		if p.end < 0 {
			// 未知大小的单流下载：无法从文件名判断完整性，检查非零。
			if fi, err := os.Stat(p.path); err != nil || fi.Size() == 0 {
				return fmt.Errorf("层 %s 下载内容为空", shortDigest(st.blob.Digest))
			}
		}
		_ = os.Remove(objectPath)
		return os.Rename(p.path, objectPath)
	}

	tmp := objectPath + ".tmp"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	for _, p := range st.parts {
		in, err := os.Open(p.path)
		if err != nil {
			out.Close()
			_ = os.Remove(tmp)
			return err
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		if copyErr != nil {
			out.Close()
			_ = os.Remove(tmp)
			return copyErr
		}
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(objectPath)
	return os.Rename(tmp, objectPath)
}

// verifyObject 校验文件摘要。onBytes 上报已读取的字节数（可为 nil）。
func (d *Downloader) verifyObject(path, digest string, onBytes func(int64)) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, countWriter{fn: onBytes}), f); err != nil {
		return false, err
	}
	actual := "sha256:" + hex.EncodeToString(h.Sum(nil))
	return actual == digest, nil
}

// countWriter 将写入的字节数上报给回调。
type countWriter struct {
	fn func(int64)
}

func (c countWriter) Write(p []byte) (int, error) {
	if c.fn != nil {
		c.fn(int64(len(p)))
	}
	return len(p), nil
}

func (d *Downloader) resetBlob(st *blobState) {
	st.verified = false
	d.opts.Progress.ResetLayer(st.progressIdx)
	_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "parts", mustHex(st.blob.Digest)))
	_ = os.MkdirAll(filepath.Join(d.opts.CacheDir, "parts", mustHex(st.blob.Digest)), 0o700)
	if p, err := ObjectPath(d.opts.CacheDir, st.blob.Digest); err == nil {
		_ = os.Remove(p)
	}
	delete(d.state.Verified, st.blob.Digest)
	// 重新生成分片计划。
	st.parts = d.makeParts(st.blob, len(st.parts) > 1)
	st.chunked = len(st.parts) > 1
}

// disableChunking 将所有未完成的层退化为单流下载计划（丢弃已有分片），
// 用于服务器中途不再支持 Range 请求的场景。
func (d *Downloader) disableChunking() {
	for _, st := range d.blobs {
		if st.verified {
			continue
		}
		d.opts.Progress.ResetLayer(st.progressIdx)
		_ = os.RemoveAll(filepath.Join(d.opts.CacheDir, "parts", mustHex(st.blob.Digest)))
		st.parts = d.makeParts(st.blob, false)
		st.chunked = false
	}
}

func mustHex(digest string) string {
	h, err := HexDigest(digest)
	if err != nil {
		return "invalid"
	}
	return h
}
