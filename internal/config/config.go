// Package config loads the process configuration from AIGC_-prefixed
// environment variables.
//
// 只从环境变量读，不读配置文件：凭证不该躺在仓库里的任何一个文件中，
// 而一旦有了配置文件，早晚会有人把带密钥的那份提交上去。
// configs/.env.example 只放占位符，供本地复制成 .env（已在 .gitignore 中）。
//
// 缺失必填项时返回错误而不是用一个"看起来能跑"的默认值兜底——
// 用默认 DSN 启动的进程会在第一个请求时才炸，而且报的是一个和配置无关的错误。
// 启动即失败，错误信息里直接写清缺哪个变量。
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrMissing 供调用方用 errors.Is 判定「配置缺项」这一类错误。
var ErrMissing = errors.New("config: missing required environment variables")

// Prefix 是全部环境变量的统一前缀。
const Prefix = "AIGC_"

// 默认值。凡是有合理默认的都给默认，只有凭证与 DSN 没有默认——
// 那两样猜错了不如不猜。
const (
	DefaultHTTPAddr        = ":18080"
	DefaultShutdownTimeout = 15 * time.Second
	DefaultReadTimeout     = 30 * time.Second
	// DefaultWriteTimeout 为 0 表示不限制写超时。这是 SSE 长连接的硬要求：
	// 任何非零的写超时都会在到点时把正在推事件的连接掐断。
	DefaultWriteTimeout      = time.Duration(0)
	DefaultIdleTimeout       = 120 * time.Second
	DefaultStorageRoot       = "./data/storage"
	DefaultPublicBaseURL     = "http://localhost:18080"
	DefaultCORSOrigins       = "*"
	DefaultJWTSecretEnv      = "AIGC_JWT_SECRET"
	DefaultJWTTTL            = 720 * time.Hour
	DefaultWorkerConcurrency = 4
	DefaultPollInterval      = 12 * time.Second
)

// MinPort 是本地端口规范的下界：本项目所有监听端口必须 >= 10000。
//
// 这不是洁癖，是一次事故的对策。2026-08-08 凌晨一个 aigcd 绑在 IPv6 的 *:8080 上，
// 而同机另一个只发布在 IPv4 127.0.0.1:8080 的服务被它静默接管——macOS 上 localhost
// 优先解析到 ::1，于是所有 localhost:8080 请求都落到 aigcd 手里，对每条路由返回
// 404，持续 5.5 小时才被发现。把默认端口抬到万位以上，是为了让本项目不再有机会
// 撞进别人的端口里。
const MinPort = 10000

// ErrPortPolicy 供调用方用 errors.Is 判定「端口不符合本地端口规范」。
var ErrPortPolicy = errors.New("config: listen port violates the local port policy")

// Config 是进程的全部配置。
type Config struct {
	// HTTPAddr 是 HTTP 监听地址，形如 ":18080"。端口受 MinPort 约束，见 validateHTTPAddr。
	HTTPAddr string
	// ShutdownTimeout 是收到终止信号后等待在途请求结束的上限。
	ShutdownTimeout time.Duration
	ReadTimeout     time.Duration
	// WriteTimeout 为 0 表示不限制，SSE 依赖这一点。
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// MySQLDSN 是 database/sql 的 MySQL DSN，必填。
	MySQLDSN string

	// StorageRoot 是 L3 资产落盘的根目录。上游产物在任务成功那一刻转存到这里。
	StorageRoot string

	// PublicBaseURL 是本服务对外可达的根地址。它有两个用途：
	// 一是拼出 Asset.Original 这类下发前端的 URL，二是把本地输入素材的地址
	// 交给上游回源拉取（图生图时上游要能访问到参考图）。
	// 本地开发时上游访问不到 localhost，这正是 mock driver 存在的另一个理由。
	PublicBaseURL string

	// CORSOrigins 是允许的前端来源，逗号分隔；"*" 表示不限制。
	// 本地前后端分离开发必须放行 Vite 的来源。
	CORSOrigins []string

	// JWTSecretEnv 是**存放密钥的那个环境变量的名字**，
	// JWTSecret 是从它读出来的值。与 Provider.CredentialRef 同一套思路：
	// 配置里出现的是变量名，密钥本身只活在环境里。
	JWTSecretEnv string
	JWTSecret    string
	JWTTTL       time.Duration

	// WorkerConcurrency 是 L2 worker 的并发数。
	WorkerConcurrency int
	// PollInterval 是轮询的默认间隔，模型配置未指定时生效。
	PollInterval time.Duration

	// StoryboardModelID 是画布对话拆分镜默认用哪个模型。
	// 留空表示「取第一个启用的 chat 族模型」——本地起一份新库时不必先配它。
	// 它存的是 models 表的 id，不是上游模型名：换供应商是改一条配置，
	// 不是改代码。
	StoryboardModelID string

	// ComposeModelID 是画布一键合成默认用哪个模型，与 StoryboardModelID 同理：
	// 留空表示「取第一个启用的 compose 族模型」。真正能转码拼接的实现接进来
	// 之后，切过去只是改这一个变量。
	ComposeModelID string
}

// Load 从环境变量读取配置。
//
// 必填项缺失时把**全部**缺失项一次性报出来，而不是报第一个就返回：
// 第一次跑起来的人不该改一个变量重试一次。
func Load() (*Config, error) {
	loadDotEnv(".env")

	var missing []string

	cfg := &Config{
		HTTPAddr:          envString("HTTP_ADDR", DefaultHTTPAddr),
		StorageRoot:       envString("STORAGE_ROOT", DefaultStorageRoot),
		PublicBaseURL:     strings.TrimRight(envString("PUBLIC_BASE_URL", DefaultPublicBaseURL), "/"),
		CORSOrigins:       splitCSV(envString("CORS_ORIGINS", DefaultCORSOrigins)),
		StoryboardModelID: envString("STORYBOARD_MODEL", ""),
		ComposeModelID:    envString("COMPOSE_MODEL", ""),
	}

	if err := validateHTTPAddr(cfg.HTTPAddr); err != nil {
		return nil, err
	}

	var err error
	if cfg.ShutdownTimeout, err = envDuration("SHUTDOWN_TIMEOUT", DefaultShutdownTimeout); err != nil {
		return nil, err
	}
	if cfg.ReadTimeout, err = envDuration("READ_TIMEOUT", DefaultReadTimeout); err != nil {
		return nil, err
	}
	if cfg.WriteTimeout, err = envDuration("WRITE_TIMEOUT", DefaultWriteTimeout); err != nil {
		return nil, err
	}
	if cfg.IdleTimeout, err = envDuration("IDLE_TIMEOUT", DefaultIdleTimeout); err != nil {
		return nil, err
	}
	if cfg.JWTTTL, err = envDuration("JWT_TTL", DefaultJWTTTL); err != nil {
		return nil, err
	}
	if cfg.PollInterval, err = envDuration("POLL_INTERVAL", DefaultPollInterval); err != nil {
		return nil, err
	}
	if cfg.WorkerConcurrency, err = envInt("WORKER_CONCURRENCY", DefaultWorkerConcurrency); err != nil {
		return nil, err
	}
	if cfg.WorkerConcurrency < 1 {
		return nil, fmt.Errorf("config: %sWORKER_CONCURRENCY must be >= 1, got %d", Prefix, cfg.WorkerConcurrency)
	}
	if cfg.PollInterval <= 0 {
		return nil, fmt.Errorf("config: %sPOLL_INTERVAL must be > 0, got %s", Prefix, cfg.PollInterval)
	}

	cfg.MySQLDSN = envString("MYSQL_DSN", "")
	if cfg.MySQLDSN == "" {
		missing = append(missing, Prefix+"MYSQL_DSN")
	}

	// 先拿到密钥变量名，再从那个变量里取密钥本身。
	cfg.JWTSecretEnv = envString("JWT_SECRET_ENV", DefaultJWTSecretEnv)
	cfg.JWTSecret = strings.TrimSpace(os.Getenv(cfg.JWTSecretEnv))
	if cfg.JWTSecret == "" {
		missing = append(missing, cfg.JWTSecretEnv+" (referenced by "+Prefix+"JWT_SECRET_ENV)")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissing, strings.Join(missing, ", "))
	}
	return cfg, nil
}

// Redacted 返回可安全打印的配置摘要。
//
// 它存在的唯一理由是启动日志：把 Config 直接打出来会把 JWTSecret 和 DSN 里的
// 密码一起写进日志文件。这里只输出不敏感的字段，密钥一律以变量名代替。
func (c *Config) Redacted() map[string]string {
	return map[string]string{
		"http_addr":          c.HTTPAddr,
		"shutdown_timeout":   c.ShutdownTimeout.String(),
		"storage_root":       c.StorageRoot,
		"public_base_url":    c.PublicBaseURL,
		"cors_origins":       strings.Join(c.CORSOrigins, ","),
		"mysql_dsn":          "(set via " + Prefix + "MYSQL_DSN)",
		"jwt_secret":         "(set via " + c.JWTSecretEnv + ")",
		"jwt_ttl":            c.JWTTTL.String(),
		"worker_concurrency": strconv.Itoa(c.WorkerConcurrency),
		"poll_interval":      c.PollInterval.String(),
		"storyboard_model":   c.StoryboardModelID,
		"compose_model":      c.ComposeModelID,
	}
}

// validateHTTPAddr 把本地端口规范变成启动时的硬约束：端口必须显式给出，
// 且必须 >= MinPort。配成 8080 这类低位端口时直接拒绝启动，而不是留一条
// 注释在文档里等人自觉。
func validateHTTPAddr(addr string) error {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: %sHTTP_ADDR is not a valid listen address (%q), want something like %q: %w",
			Prefix, addr, DefaultHTTPAddr, err)
	}
	// 端口留空会让内核随机分配，进程实际听在哪没人知道，前端代理也就配不出来。
	if portStr == "" {
		return fmt.Errorf("%w: %sHTTP_ADDR (%q) must name an explicit port, want something like %q",
			ErrPortPolicy, Prefix, addr, DefaultHTTPAddr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("config: %sHTTP_ADDR port is not a number (%q)", Prefix, portStr)
	}
	if port < MinPort {
		return fmt.Errorf("%w: %sHTTP_ADDR (%q) uses port %d, but this project requires >= %d; use %q instead",
			ErrPortPolicy, Prefix, addr, port, MinPort, DefaultHTTPAddr)
	}
	return nil
}

// envString 读取一个带前缀的字符串变量，未设置或为空时返回 def。
func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(Prefix + key)); v != "" {
		return v
	}
	return def
}

// loadDotEnv 把项目根的 .env 灌进进程环境，**不覆盖已存在的变量**。
//
// 这与"只从环境变量读"并不矛盾：.env 已在 .gitignore 里，它是开发者本机
// 那份环境的落盘形态，不是提交进仓库的配置文件。不做这件事，`make run`
// 就得写成一长串 env 前缀，而 README 的第一条命令越长越没人照着走。
// 已设置的变量优先，是为了让 `AIGC_HTTP_ADDR=:9090 make run` 这种临时覆盖生效。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, val)
	}
}

// envDuration 读取一个 time.ParseDuration 形态的变量（如 "15s"、"720h"）。
func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(Prefix + key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s%s is not a valid duration (%q): %w", Prefix, key, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: %s%s must not be negative, got %s", Prefix, key, d)
	}
	return d, nil
}

// envInt 读取一个十进制整数变量。
func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(Prefix + key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s%s is not a valid integer (%q): %w", Prefix, key, raw, err)
	}
	return n, nil
}

// splitCSV 把逗号分隔的列表切成去空白、去空项的切片。
func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
