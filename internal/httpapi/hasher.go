package httpapi

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrPasswordMismatch 是口令比对失败的原因。
var ErrPasswordMismatch = errors.New("httpapi: password mismatch")

// pbkdf2Params 是当前的算法参数。
//
// 用 PBKDF2-HMAC-SHA256 而不是 bcrypt/argon2：Go 1.24 起 crypto/pbkdf2 进了标准库，
// 而 bcrypt 与 argon2 都在 golang.org/x/crypto 里。本项目除 MySQL 驱动外不引
// 第三方依赖，PBKDF2 是标准库里唯一的口令哈希原语，且 600k 轮是 OWASP 对
// PBKDF2-SHA256 的当前建议值。
//
// 参数升级时只加常量、不改旧哈希：存储格式带着自己的参数，
// NeedsRehash 在用户下次成功登录时触发平滑迁移。
const (
	pbkdf2Iterations = 600_000
	pbkdf2SaltBytes  = 16
	pbkdf2KeyBytes   = 32
	pbkdf2Prefix     = "pbkdf2_sha256"
)

// pbkdf2Hasher 是 PasswordHasher 的实现。
//
// 存储格式: pbkdf2_sha256$<iterations>$<base64 salt>$<base64 key>
// 自带参数，因此换参数不需要迁移历史数据。
type pbkdf2Hasher struct{}

// NewPasswordHasher 返回默认的口令哈希器。
func NewPasswordHasher() PasswordHasher { return pbkdf2Hasher{} }

func (pbkdf2Hasher) Hash(plain string) (string, error) {
	salt := make([]byte, pbkdf2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, plain, salt, pbkdf2Iterations, pbkdf2KeyBytes)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Prefix, pbkdf2Iterations,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func (pbkdf2Hasher) Compare(hash, plain string) error {
	iter, salt, want, err := parsePBKDF2(hash)
	if err != nil {
		return err
	}
	got, err := pbkdf2.Key(sha256.New, plain, salt, iter, len(want))
	if err != nil {
		return err
	}
	// 恒定时间比较，理由同 JWT 签名校验。
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

func (pbkdf2Hasher) NeedsRehash(hash string) bool {
	iter, _, key, err := parsePBKDF2(hash)
	if err != nil {
		return true
	}
	return iter < pbkdf2Iterations || len(key) != pbkdf2KeyBytes
}

func parsePBKDF2(hash string) (iter int, salt, key []byte, err error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Prefix {
		return 0, nil, nil, ErrPasswordMismatch
	}
	if iter, err = strconv.Atoi(parts[1]); err != nil || iter < 1 {
		return 0, nil, nil, ErrPasswordMismatch
	}
	enc := base64.RawStdEncoding
	if salt, err = enc.DecodeString(parts[2]); err != nil {
		return 0, nil, nil, ErrPasswordMismatch
	}
	if key, err = enc.DecodeString(parts[3]); err != nil {
		return 0, nil, nil, ErrPasswordMismatch
	}
	return iter, salt, key, nil
}
