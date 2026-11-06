package registry

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
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxManifestSize = 16 << 20 // 16 MiB

// statusError 表示仓库返回了非 2xx 状态码。
type statusError struct {
	Code int
	Op   string
	Body string
}

func (e *statusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s失败（HTTP %d）", e.Op, e.Code)
	}
	return fmt.Sprintf("%s失败（HTTP %d）: %s", e.Op, e.Code, e.Body)
}

// retryable 判断错误是否值得重试。
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests ||
			se.Code == http.StatusRequestTimeout ||
			se.Code >= 500
	}
	return true
}

// retry 执行带指数退避的重试。
func (c *Client) retry(ctx context.Context, fn func() error) error {
	attempts := c.opts.Retries + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = fn()
		if last == nil {
			return nil
		}
		if !retryable(last) || i == attempts-1 {
			return last
		}
		wait := time.Duration(1<<min(i, 5)) * 500 * time.Millisecond
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		c.opts.Log("请求失败，%s 后重试 (%d/%d): %v", wait, i+1, attempts-1, last)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return last
}

// Options 是创建 Registry 客户端的参数。
type Options struct {
	Registry    string
	Repository  string
	Credentials Credentials
	HTTPClient  *http.Client
	UserAgent   string
	PlainHTTP   bool
	// Retries 是单个请求的最大重试次数，0 表示不重试。
	Retries int
	Verbose func(format string, args ...interface{})
	// Log 输出关键过程信息（认证挑战、获取令牌、请求重试等），默认不输出。
	Log func(format string, args ...interface{})
}

// Client 是一个仓库维度的 Registry API 客户端，内部缓存访问令牌。
type Client struct {
	opts Options
	base string

	mu         sync.RWMutex
	authHeader string
	authExpiry time.Time
	challenge  *challenge

	refreshMu sync.Mutex
}

// New 创建 Registry 客户端。
func New(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "go-docker-pull-tar"
	}
	if opts.Verbose == nil {
		opts.Verbose = func(string, ...interface{}) {}
	}
	if opts.Log == nil {
		opts.Log = func(string, ...interface{}) {}
	}
	scheme := "https"
	if opts.PlainHTTP {
		scheme = "http"
	}
	return &Client{
		opts: opts,
		base: scheme + "://" + opts.Registry,
	}
}

// Registry 返回仓库地址。
func (c *Client) Registry() string { return c.opts.Registry }

// Repository 返回仓库路径。
func (c *Client) Repository() string { return c.opts.Repository }

// newRequest 创建带有通用请求头的请求。
func (c *Client) newRequest(ctx context.Context, method, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	return req, nil
}

// do 执行请求并在遇到 401 时自动完成认证后重试。
func (c *Client) do(req *http.Request) (*http.Response, error) {
	for attempt := 0; attempt < 3; attempt++ {
		c.mu.RLock()
		header := c.authHeader
		c.mu.RUnlock()
		applyAuth(req, header)

		resp, err := c.opts.HTTPClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}

		ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		// 401 响应体需要关闭后才能复用连接。
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()

		if ch == nil {
			if attempt > 0 {
				return nil, fmt.Errorf("仓库 %s 要求认证，但未返回 WWW-Authenticate 头", c.opts.Registry)
			}
			// 没有挑战信息时尝试直接使用 Basic 认证。
			if c.opts.Credentials.Empty() {
				return nil, fmt.Errorf("仓库 %s 要求认证，请通过 -username/-password 提供凭据", c.opts.Registry)
			}
			c.opts.Log("仓库 %s 要求认证（未返回 WWW-Authenticate），尝试 Basic 认证", c.opts.Registry)
			c.mu.Lock()
			c.authHeader = basicAuthHeader(c.opts.Credentials)
			c.authExpiry = time.Now().Add(5 * time.Minute)
			c.mu.Unlock()
			continue
		}

		c.opts.Log("收到认证挑战: %s realm=%q service=%q", ch.Scheme, ch.Realm, ch.Service)

		// 若刚被拒绝的正是当前缓存的认证头，说明凭据已被吊销，强制刷新；
		// 反之可能已有并发请求完成刷新，直接复用新凭据即可。
		c.mu.RLock()
		force := header != "" && header == c.authHeader
		c.mu.RUnlock()
		if err := c.refreshAuth(req.Context(), ch, force); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("仓库 %s 认证失败", c.opts.Registry)
}

// refreshAuth 根据认证挑战获取新的认证头。
func (c *Client) refreshAuth(ctx context.Context, ch *challenge, force bool) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// 其他并发请求可能已经完成刷新。
	if !force {
		c.mu.RLock()
		already := c.authHeader != "" && time.Now().Before(c.authExpiry) &&
			c.challenge != nil && c.challenge.Realm == ch.Realm
		c.mu.RUnlock()
		if already {
			return nil
		}
	}

	var header string
	var expiry time.Time

	switch ch.Scheme {
	case "basic":
		if c.opts.Credentials.Empty() {
			return fmt.Errorf("仓库 %s 要求 Basic 认证，请提供用户名和密码", c.opts.Registry)
		}
		header = basicAuthHeader(c.opts.Credentials)
		expiry = time.Now().Add(10 * time.Minute)

	case "bearer", "token", "":
		if ch.Realm == "" {
			if !c.opts.Credentials.Empty() {
				header = basicAuthHeader(c.opts.Credentials)
				expiry = time.Now().Add(10 * time.Minute)
				break
			}
			return fmt.Errorf("仓库 %s 的认证挑战缺少 realm", c.opts.Registry)
		}
		token, exp, err := c.fetchToken(ctx, ch)
		if err != nil {
			return err
		}
		header = "Bearer " + token
		expiry = exp

	default:
		return fmt.Errorf("不支持的认证方式: %s", ch.Scheme)
	}

	c.mu.Lock()
	c.authHeader = header
	c.authExpiry = expiry
	c.challenge = ch
	c.mu.Unlock()

	c.opts.Log("认证成功: scheme=%s service=%s", ch.Scheme, ch.Service)
	return nil
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	IssuedAt    string `json:"issued_at"`
}

// fetchToken 向令牌服务换取 Bearer Token。
func (c *Client) fetchToken(ctx context.Context, ch *challenge) (string, time.Time, error) {
	realm := ch.Realm
	u, err := url.Parse(realm)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("无效的认证地址 %q: %w", realm, err)
	}
	q := u.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	scope := ch.Scope
	if !strings.Contains(scope, "repository:") {
		scope = fmt.Sprintf("repository:%s:pull", c.opts.Repository)
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	c.opts.Log("正在获取访问令牌: %s", u.Redacted())

	var token string
	var expiry time.Time
	err = c.retry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.opts.UserAgent)
		req.Header.Set("Accept", "application/json")
		if !c.opts.Credentials.Empty() {
			req.SetBasicAuth(c.opts.Credentials.Username, c.opts.Credentials.Password)
		}

		resp, err := c.opts.HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("请求认证服务失败: %w", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return fmt.Errorf("读取认证响应失败: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			msg := strings.TrimSpace(string(body))
			if len(msg) > 300 {
				msg = msg[:300]
			}
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				return &statusError{Code: resp.StatusCode, Op: "认证", Body: "请检查用户名和密码: " + msg}
			}
			return &statusError{Code: resp.StatusCode, Op: "认证服务", Body: msg}
		}

		var tr tokenResponse
		if err := json.Unmarshal(body, &tr); err != nil {
			return fmt.Errorf("解析认证响应失败: %w", err)
		}
		token = tr.Token
		if token == "" {
			token = tr.AccessToken
		}
		if token == "" {
			return fmt.Errorf("认证响应中缺少 token 字段")
		}

		ttl := time.Duration(tr.ExpiresIn) * time.Second
		if ttl <= 0 {
			// 规范规定省略 expires_in 时默认 60 秒。
			ttl = 60 * time.Second
		}
		// 提前 30 秒过期，避免临界时刻失败。
		expiry = time.Now().Add(ttl - 30*time.Second)
		if expiry.Before(time.Now()) {
			expiry = time.Now().Add(time.Minute)
		}
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiry, nil
}

// FetchManifest 拉取清单，返回原始内容、媒体类型和 sha256 摘要。
func (c *Client) FetchManifest(ctx context.Context, reference string) ([]byte, string, string, error) {
	rawURL := fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, c.opts.Repository, reference)

	var body []byte
	var mediaType string
	err := c.retry(ctx, func() error {
		req, err := c.newRequest(ctx, http.MethodGet, rawURL)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", AcceptHeader)

		c.opts.Verbose("GET %s", rawURL)
		resp, err := c.do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return &statusError{Code: resp.StatusCode, Op: "获取清单", Body: strings.TrimSpace(string(data))}
		}

		body, err = io.ReadAll(io.LimitReader(resp.Body, maxManifestSize))
		if err != nil {
			return fmt.Errorf("读取清单失败: %w", err)
		}
		mediaType = resp.Header.Get("Content-Type")
		if i := strings.IndexByte(mediaType, ';'); i >= 0 {
			mediaType = strings.TrimSpace(mediaType[:i])
		}
		return nil
	})
	if err != nil {
		return nil, "", "", err
	}
	sum := sha256.Sum256(body)
	return body, mediaType, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// BlobRequest 创建 Blob 请求。start/end 为闭区间；end < 0 表示不限制。
// 调用方负责关闭响应体。
func (c *Client) BlobRequest(ctx context.Context, digest string, start, end int64) (*http.Response, error) {
	rawURL := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, c.opts.Repository, digest)
	req, err := c.newRequest(ctx, http.MethodGet, rawURL)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if start >= 0 {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}

	c.opts.Verbose("GET %s range=%d-%d", rawURL, start, end)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusPartialContent &&
		resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, &statusError{
			Code: resp.StatusCode,
			Op:   fmt.Sprintf("下载 Blob %s", digest),
			Body: strings.TrimSpace(string(body)),
		}
	}
	return resp, nil
}

// HeadBlob 获取 Blob 大小。失败时返回 0。
func (c *Client) HeadBlob(ctx context.Context, digest string) int64 {
	rawURL := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, c.opts.Repository, digest)
	req, err := c.newRequest(ctx, http.MethodHead, rawURL)
	if err != nil {
		return 0
	}
	resp, err := c.do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	return parseContentLength(resp.Header.Get("Content-Length"))
}

// ProbeRange 探测服务器是否支持 Range 请求，并顺带获取 Blob 总大小。
func (c *Client) ProbeRange(ctx context.Context, digest string) (size int64, supported bool) {
	resp, err := c.BlobRequest(ctx, digest, 0, 0)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode == http.StatusPartialContent {
		if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			return total, true
		}
		return 0, true
	}
	return parseContentLength(resp.Header.Get("Content-Length")), false
}

// FetchBlob 读取完整的 Blob 内容（用于较小的配置对象）。
func (c *Client) FetchBlob(ctx context.Context, digest string, maxSize int64) ([]byte, error) {
	var body []byte
	err := c.retry(ctx, func() error {
		resp, err := c.BlobRequest(ctx, digest, -1, -1)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &statusError{Code: resp.StatusCode, Op: fmt.Sprintf("获取 Blob %s", digest)}
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
		if err != nil {
			return fmt.Errorf("读取 Blob 失败: %w", err)
		}
		if int64(len(body)) > maxSize {
			return fmt.Errorf("Blob %s 超过大小限制", digest)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != digest {
		return nil, fmt.Errorf("Blob 校验失败: 期望 %s，实际 %s", digest, actual)
	}
	return body, nil
}

// FetchManifestDigest 按摘要拉取并校验清单。
func (c *Client) FetchManifestDigest(ctx context.Context, digest string) ([]byte, string, error) {
	body, mediaType, actual, err := c.FetchManifest(ctx, digest)
	if err != nil {
		return nil, "", err
	}
	if digest != "" && actual != digest {
		return nil, "", fmt.Errorf("清单校验失败: 期望 %s，实际 %s", digest, actual)
	}
	return body, mediaType, nil
}

func parseContentLength(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseContentRangeTotal 从 "bytes 0-0/12345" 中解析总大小。
func parseContentRangeTotal(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	i := strings.LastIndexByte(s, '/')
	if i < 0 {
		return 0
	}
	return parseContentLength(s[i+1:])
}
