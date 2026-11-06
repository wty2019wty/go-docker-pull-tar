package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Credentials 是仓库登录凭据。
type Credentials struct {
	Username string
	Password string
}

// Empty 判断凭据是否为空。
func (c Credentials) Empty() bool {
	return c.Username == "" && c.Password == ""
}

// challenge 是解析后的 WWW-Authenticate 认证挑战。
type challenge struct {
	Scheme  string
	Realm   string
	Service string
	Scope   string
}

var challengeParamRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_-]*)\s*=\s*(?:"([^"]*)"|([^\s,]+))`)

// parseChallenge 解析 WWW-Authenticate 头。
func parseChallenge(header string) *challenge {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	c := &challenge{}
	header = strings.TrimSpace(header)
	if i := strings.IndexAny(header, " \t"); i > 0 {
		c.Scheme = strings.ToLower(header[:i])
	} else {
		c.Scheme = strings.ToLower(header)
		return c
	}
	for _, m := range challengeParamRe.FindAllStringSubmatch(header, -1) {
		key := strings.ToLower(m[1])
		val := m[2]
		if val == "" {
			val = m[3]
		}
		switch key {
		case "realm":
			c.Realm = val
		case "service":
			c.Service = val
		case "scope":
			c.Scope = val
		}
	}
	return c
}

// basicAuthHeader 生成 Basic 认证头。
func basicAuthHeader(c Credentials) string {
	raw := c.Username + ":" + c.Password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

// dockerConfig 是 ~/.docker/config.json 中本工具关心的部分。
type dockerConfig struct {
	Auths       map[string]dockerAuth `json:"auths"`
	CredsStore  string                `json:"credsStore"`
	CredHelpers map[string]string     `json:"credHelpers"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// LoadDockerCredentials 从 Docker CLI 配置文件中读取指定仓库的凭据。
// 找不到时返回空凭据，不视为错误。
func LoadDockerCredentials(registryHost string) (Credentials, error) {
	cfgPath := os.Getenv("DOCKER_CONFIG")
	if cfgPath != "" {
		cfgPath = filepath.Join(cfgPath, "config.json")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return Credentials{}, nil
		}
		cfgPath = filepath.Join(home, ".docker", "config.json")
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, nil
		}
		return Credentials{}, err
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Credentials{}, fmt.Errorf("解析 Docker 配置 %s 失败: %w", cfgPath, err)
	}

	want := normalizeRegistryKey(registryHost)
	for key, auth := range cfg.Auths {
		if normalizeRegistryKey(key) != want {
			continue
		}
		if auth.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
			if err != nil {
				return Credentials{}, fmt.Errorf("解码 Docker 配置中的 auth 失败: %w", err)
			}
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				return Credentials{Username: parts[0], Password: parts[1]}, nil
			}
		}
		if auth.Username != "" || auth.Password != "" {
			return Credentials{Username: auth.Username, Password: auth.Password}, nil
		}
	}

	return Credentials{}, nil
}

// normalizeRegistryKey 将仓库地址归一化，便于匹配 Docker 配置中的键。
func normalizeRegistryKey(key string) string {
	k := strings.TrimSpace(strings.ToLower(key))
	k = strings.TrimPrefix(k, "https://")
	k = strings.TrimPrefix(k, "http://")
	k = strings.TrimSuffix(k, "/")
	k = strings.TrimSuffix(k, "/v1")
	switch k {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "index.docker.io/v1", "registry-1.docker.io/v2":
		return "docker.io"
	}
	return k
}

// applyAuth 为请求附加认证头。
func applyAuth(req *http.Request, header string) {
	if header == "" {
		return
	}
	req.Header.Set("Authorization", header)
}
