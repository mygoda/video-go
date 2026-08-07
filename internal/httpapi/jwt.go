package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// ErrInvalidToken 是所有令牌校验失败的统一原因。
//
// 刻意不区分「签名错」「过期了」「格式不对」：对调用方而言这三者都只有一个
// 处置方式（重新登录），而把区别回传给客户端等于给伪造者一个渐进式的提示。
var ErrInvalidToken = errors.New("httpapi: invalid token")

// jwtIssuer 是 TokenIssuer 的 HS256 实现，只用标准库。
//
// 不引 JWT 库的理由：这里用到的是 JWT 的最小子集（HS256、三段式、固定几个
// claim），标准库的 crypto/hmac + encoding/json 就是全部所需，
// 而一个第三方库会带来一整套它自己的算法协商与解析面。
type jwtIssuer struct {
	secret []byte
	now    func() time.Time
}

// NewJWTIssuer 用给定密钥构造签发器。密钥来自 config.Config.JWTSecret（读自 env）。
func NewJWTIssuer(secret string, now func() time.Time) TokenIssuer {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &jwtIssuer{secret: []byte(secret), now: now}
}

// jwtClaims 是载荷。字段名沿用 JWT 习惯，便于用通用工具排查。
type jwtClaims struct {
	Sub  string `json:"sub"`
	Role string `json:"role"`
	Exp  int64  `json:"exp"`
	Iat  int64  `json:"iat"`
	Jti  string `json:"jti"`
}

func (j *jwtIssuer) Issue(userID string, role domain.Role, ttl time.Duration) (string, time.Time, error) {
	now := j.now().UTC()
	exp := now.Add(ttl)

	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", time.Time{}, err
	}
	payload, err := json.Marshal(jwtClaims{
		Sub:  userID,
		Role: string(role),
		Exp:  exp.Unix(),
		Iat:  now.Unix(),
		Jti:  uid.Token(8),
	})
	if err != nil {
		return "", time.Time{}, err
	}

	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	return signing + "." + enc.EncodeToString(j.sign(signing)), exp, nil
}

func (j *jwtIssuer) Verify(token string) (string, domain.Role, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", ErrInvalidToken
	}
	enc := base64.RawURLEncoding

	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return "", "", ErrInvalidToken
	}
	// hmac.Equal 是恒定时间比较：普通的 bytes.Equal 会在第一个不同的字节处返回，
	// 攻击者能靠计时逐字节试出签名。
	if !hmac.Equal(sig, j.sign(parts[0]+"."+parts[1])) {
		return "", "", ErrInvalidToken
	}

	raw, err := enc.DecodeString(parts[1])
	if err != nil {
		return "", "", ErrInvalidToken
	}
	var c jwtClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", "", ErrInvalidToken
	}
	if c.Sub == "" {
		return "", "", ErrInvalidToken
	}
	if j.now().UTC().Unix() >= c.Exp {
		return "", "", fmt.Errorf("%w: expired", ErrInvalidToken)
	}

	role := domain.Role(c.Role)
	if role != domain.RoleAdmin {
		// 未知角色一律降级为普通用户，绝不因为解析不出来就放行成 admin。
		role = domain.RoleUser
	}
	return c.Sub, role, nil
}

func (j *jwtIssuer) sign(signing string) []byte {
	m := hmac.New(sha256.New, j.secret)
	m.Write([]byte(signing))
	return m.Sum(nil)
}
