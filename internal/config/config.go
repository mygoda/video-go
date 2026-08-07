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
	"errors"
	"fmt"
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
	DefaultHTTPAddr        = ":8080"
	DefaultShutdownTimeout = 15 * time.Second
	DefaultReadTimeout     = 30 * time.Second
	// DefaultWriteTimeout 为 0 表示不限制写超时。这是 SSE 长连接的硬要求：
	// 任何非零的写超时都会在到点时把正在推事件的连接掐断。
	DefaultWriteTimeout      = time.Duration(0)
	DefaultIdleTimeout       = 120 * time.Second
	DefaultStorageRoot       = "./data/storage"
	DefaultJWTSecretEnv      = "AIGC_JWT_SECRET"
	DefaultJWTTTL            = 720 * time.Hour
	DefaultWorkerConcurrency = 4
	DefaultPollInterval      = 12 * time.Second
)

// Config 是进程的全部配置。
type Config struct {
	// HTTPAddr 是 HTTP 监听地址，形如 ":8080"。
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
}

// Load 从环境变量读取配置。
//
// 必填项缺失时把**全部**缺失项一次性报出来，而不是报第一个就返回：
// 第一次跑起来的人不该改一个变量重试一次。
func Load() (*Config, error) {
	var missing []string

	cfg := &Config{
		HTTPAddr:    envString("HTTP_ADDR", DefaultHTTPAddr),
		StorageRoot: envString("STORAGE_ROOT", DefaultStorageRoot),
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
		"mysql_dsn":          "(set via " + Prefix + "MYSQL_DSN)",
		"jwt_secret":         "(set via " + c.JWTSecretEnv + ")",
		"jwt_ttl":            c.JWTTTL.String(),
		"worker_concurrency": strconv.Itoa(c.WorkerConcurrency),
		"poll_interval":      c.PollInterval.String(),
	}
}

// envString 读取一个带前缀的字符串变量，未设置或为空时返回 def。
func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(Prefix + key)); v != "" {
		return v
	}
	return def
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
