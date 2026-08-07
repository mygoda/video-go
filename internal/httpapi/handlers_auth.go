package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// registerInitialCredits 是新注册账号的起始额度。
//
// 不接支付网关（PRD 定的），所以注册就送一笔，否则新用户第一次点生成
// 就撞积分不足，连"这个平台能干什么"都看不到。后续充值走管理端。
const registerInitialCredits = 1000

// minPasswordLength 与 openapi.yaml 里 new_password 的 minLength 保持一致。
const minPasswordLength = 8

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authResponse struct {
	Token     string      `json:"token"`
	ExpiresAt time.Time   `json:"expires_at"`
	User      domain.User `json:"user"`
}

func (s *server) issueSession(w http.ResponseWriter, r *http.Request, u domain.User, status int) {
	token, expiresAt, err := s.deps.Tokens.Issue(u.ID, u.Role, s.deps.Config.JWTTTL)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}
	writeJSON(w, status, authResponse{
		Token:     token,
		ExpiresAt: expiresAt.UTC(),
		User:      u,
	})
}

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "username", Message: "用户名不能为空"}}, "参数校验未通过"))
		return
	}
	if len(req.Password) < minPasswordLength {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "password", Message: "密码至少 8 位"}}, "参数校验未通过"))
		return
	}

	hash, err := s.deps.Hasher.Hash(req.Password)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}

	u, err := s.deps.Users.Create(r.Context(), domain.User{
		Username:     username,
		PasswordHash: hash,
		Role:         domain.RoleUser,
		Status:       domain.UserStatusActive,
		Credits:      registerInitialCredits,
		CreatedAt:    s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.issueSession(w, r, u, http.StatusCreated)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	u, err := s.deps.Users.GetByUsername(r.Context(), strings.TrimSpace(req.Username))
	if err != nil {
		// 用户不存在与密码错误回同一句话：区分开来等于送一个用户名枚举接口。
		writeError(w, r, errUnauthorized("用户名或密码错误"))
		return
	}
	if err := s.deps.Hasher.Compare(u.PasswordHash, req.Password); err != nil {
		writeError(w, r, errUnauthorized("用户名或密码错误"))
		return
	}
	if u.Status == domain.UserStatusDisabled {
		writeError(w, r, errForbidden("账号已被停用"))
		return
	}

	// 哈希参数升级后的平滑迁移：这次登录顺手用新参数重算一遍。
	// 失败不影响登录——用户拿旧哈希照样能进，只是下次再试一次。
	if s.deps.Hasher.NeedsRehash(u.PasswordHash) {
		if next, err := s.deps.Hasher.Hash(req.Password); err == nil {
			_ = s.deps.Users.UpdatePassword(r.Context(), u.ID, next)
		}
	}

	s.issueSession(w, r, u, http.StatusOK)
}

func (s *server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	u, err := s.deps.Users.GetByID(r.Context(), id.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if len(req.NewPassword) < minPasswordLength {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "new_password", Message: "密码至少 8 位"}}, "参数校验未通过"))
		return
	}

	u, err := s.deps.Users.GetByID(r.Context(), id.UserID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.deps.Hasher.Compare(u.PasswordHash, req.OldPassword); err != nil {
		writeError(w, r, errUnauthorized("原密码不正确"))
		return
	}
	hash, err := s.deps.Hasher.Hash(req.NewPassword)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}
	if err := s.deps.Users.UpdatePassword(r.Context(), id.UserID, hash); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleMyCreditLedger(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cursor, limit := pagination(r)
	page, err := s.deps.Ledgers.List(r.Context(), id.UserID, cursor, limit)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pageOf(page.Items, page.NextCursor))
}
