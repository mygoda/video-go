package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// ── 供应商 ──────────────────────────────────────────────────────

type providerUpsert struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	BaseURL        string           `json:"base_url"`
	CredentialRef  string           `json:"credential_ref"`
	AuthStyle      domain.AuthStyle `json:"auth_style"`
	AuthHeaderName string           `json:"auth_header_name"`
	Enabled        *bool            `json:"enabled"`
	TimeoutMS      int              `json:"timeout_ms"`
	MaxConcurrency int              `json:"max_concurrency"`
}

func (p providerUpsert) toDomain() (domain.Provider, error) {
	var fields []domain.FieldError
	if strings.TrimSpace(p.Name) == "" {
		fields = append(fields, domain.FieldError{Key: "name", Message: "必填"})
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		fields = append(fields, domain.FieldError{Key: "base_url", Message: "必填"})
	}
	// credential_ref 存的是**环境变量名**，不是密钥本身。填成密钥的话它会
	// 原样进库、进管理端列表、进日志——这个字段是密钥泄漏最容易发生的地方。
	if strings.TrimSpace(p.CredentialRef) == "" {
		fields = append(fields, domain.FieldError{
			Key: "credential_ref", Message: "必填，且必须是环境变量名而不是密钥本身"})
	}
	if len(fields) > 0 {
		return domain.Provider{}, errFields(fields, "参数校验未通过")
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	return domain.Provider{
		ID:             p.ID,
		Name:           strings.TrimSpace(p.Name),
		BaseURL:        strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"),
		CredentialRef:  strings.TrimSpace(p.CredentialRef),
		AuthStyle:      p.AuthStyle,
		AuthHeaderName: p.AuthHeaderName,
		Enabled:        enabled,
		TimeoutMS:      p.TimeoutMS,
		MaxConcurrency: p.MaxConcurrency,
	}, nil
}

func (s *server) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	items, err := s.deps.Providers.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if items == nil {
		items = []domain.Provider{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": items})
}

func (s *server) handleAdminCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req providerUpsert
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	p, err := req.toDomain()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := s.deps.Providers.Upsert(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) handleAdminGetProvider(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "providerId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := s.deps.Providers.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) handleAdminUpdateProvider(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "providerId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req providerUpsert
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	req.ID = id
	p, err := req.toDomain()
	if err != nil {
		writeError(w, r, err)
		return
	}
	out, err := s.deps.Providers.Upsert(r.Context(), p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "providerId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 仍被模型引用时仓储回 409：级联删会把引用它的模型变成一条永远调用失败的死配置。
	if err := s.deps.Providers.Delete(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── 模型 ────────────────────────────────────────────────────────

type modelUpsert struct {
	ID                  string                `json:"id"`
	ProviderID          string                `json:"provider_id"`
	UpstreamModel       string                `json:"upstream_model"`
	Family              domain.ProtocolFamily `json:"protocol_family"`
	VideoProtocol       *domain.VideoProtocol `json:"video_protocol"`
	Capability          json.RawMessage       `json:"capability"`
	RequestMapping      json.RawMessage       `json:"request_mapping"`
	PollIntervalSeconds *int                  `json:"poll_interval_seconds"`
	Enabled             *bool                 `json:"enabled"`
	// Visibility 缺省为空串，仓储补成 public。要把一个模型从用户端目录里
	// 摘掉（QA 夹具、画布内部用的合成模型）就显式传 internal。
	Visibility domain.ModelVisibility `json:"visibility"`
}

// handleAdminCreateModel 是「接一个走已知协议的新模型 = 加一条记录」的落点。
//
// 它一个模型名都不认识：把 capability 当数据校验一遍、确认协议族有对应 driver，
// 剩下的原样入库。GET /api/models 下一次请求就能读到（配置热生效，
// 因为那个端点每次都回源查库，不缓存在进程里）。
func (s *server) handleAdminCreateModel(w http.ResponseWriter, r *http.Request) {
	s.upsertModel(w, r, "", http.StatusCreated)
}

func (s *server) handleAdminUpdateModel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "modelId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.upsertModel(w, r, id, http.StatusOK)
}

func (s *server) upsertModel(w http.ResponseWriter, r *http.Request, forceID string, okStatus int) {
	var req modelUpsert
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if forceID != "" {
		req.ID = forceID
	}

	var fields []domain.FieldError
	if strings.TrimSpace(req.ID) == "" {
		fields = append(fields, domain.FieldError{Key: "id", Message: "必填"})
	}
	if strings.TrimSpace(req.ProviderID) == "" {
		fields = append(fields, domain.FieldError{Key: "provider_id", Message: "必填"})
	}
	if strings.TrimSpace(req.UpstreamModel) == "" {
		fields = append(fields, domain.FieldError{Key: "upstream_model", Message: "必填"})
	}
	if len(req.Capability) == 0 {
		fields = append(fields, domain.FieldError{Key: "capability", Message: "必填"})
	}
	if len(fields) > 0 {
		writeError(w, r, errFields(fields, "参数校验未通过"))
		return
	}

	schema, err := capability.DecodeSchema([]byte(req.Capability))
	if err != nil {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "capability", Message: err.Error()}}, "capability 解析失败"))
		return
	}
	// 落库前先自检 schema：一条 condition 引用了不存在的参数、一个 select
	// 没有默认值，这类错要在管理员按保存的那一刻暴露，而不是等真实用户提交时
	// 才变成一个看不懂的 invalid_param。
	if bad := s.deps.Validator.ValidateSchema(schema); len(bad) > 0 {
		writeError(w, r, errFields(bad, "capability 自检未通过"))
		return
	}

	m := domain.ModelConfig{
		ID:                  strings.TrimSpace(req.ID),
		ProviderID:          strings.TrimSpace(req.ProviderID),
		UpstreamModel:       strings.TrimSpace(req.UpstreamModel),
		Family:              req.Family,
		VideoProtocol:       req.VideoProtocol,
		Capability:          json.RawMessage(req.Capability),
		PollIntervalSeconds: req.PollIntervalSeconds,
		Enabled:             req.Enabled == nil || *req.Enabled,
		Visibility:          req.Visibility,
		UpdatedAt:           s.now(),
	}
	if len(req.RequestMapping) > 0 {
		m.RequestMapping = json.RawMessage(req.RequestMapping)
	}

	// 没有对应 driver 的配置存进去也调不通，此刻拒掉比让用户提交时失败好。
	// 注意这里问的是注册表"有没有这个名字"，不是"这是哪家供应商"——
	// DriverName 是配置到 driver 的唯一翻译点，本层不认识任何具体供应商。
	name := adapter.DriverName(m)
	if !driverRegistered(s.deps.Drivers, name) {
		writeError(w, r, errFields([]domain.FieldError{
			{Key: "protocol_family", Message: "没有名为 " + name + " 的 driver"},
		}, "参数校验未通过"))
		return
	}

	out, err := s.deps.Models.Upsert(r.Context(), m)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, okStatus, out)
}

func driverRegistered(reg adapter.Registry, name string) bool {
	if reg == nil {
		return false
	}
	for _, n := range reg.Names() {
		if n == name {
			return true
		}
	}
	return false
}

func (s *server) handleAdminListModels(w http.ResponseWriter, r *http.Request) {
	var f domain.ModelFilter
	if m := r.URL.Query().Get("modality"); m != "" {
		f.Modality = domain.Modality(m)
	}
	if e := r.URL.Query().Get("enabled"); e != "" {
		v := e == "true" || e == "1"
		f.Enabled = &v
	}
	if v := r.URL.Query().Get("visibility"); v != "" {
		f.Visibility = domain.ModelVisibility(v)
	}
	items, err := s.deps.Models.List(r.Context(), f)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if items == nil {
		items = []domain.ModelConfig{}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (s *server) handleAdminGetModel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "modelId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	m, err := s.deps.Models.Get(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleAdminDeleteModel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "modelId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.deps.Models.Delete(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type probeResult struct {
	OK              bool   `json:"ok"`
	DryRun          bool   `json:"dry_run"`
	RenderedRequest any    `json:"rendered_request,omitempty"`
	UpstreamStatus  int    `json:"upstream_status,omitempty"`
	Error           string `json:"error,omitempty"`
	LatencyMS       int64  `json:"latency_ms"`
}

// handleAdminProbeModel 用最小参数走一次 driver，验证配置是否可用。
//
// **不扣积分、不产生 asset、不落任务行。** 它是配置的自检工具，
// 走一遍真实任务流程会把管理员的每次试错都变成一条脏数据。
//
// dry_run（默认 true）只渲染请求体不发出去：没有凭证时它仍然有用——
// 能告诉管理员 request_mapping 写对没有，而这正是零凭证环境下最需要的那个答案。
func (s *server) handleAdminProbeModel(w http.ResponseWriter, r *http.Request) {
	modelID, err := pathID(r, "modelId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	req := struct {
		DryRun *bool `json:"dry_run"`
	}{}
	// 空 body 是合法的（全走默认值），解析失败不当错误。
	_ = decodeJSON(w, r, &req)
	dryRun := req.DryRun == nil || *req.DryRun

	m, err := s.deps.Models.Get(r.Context(), modelID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	provider, err := s.deps.Providers.Get(r.Context(), m.ProviderID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	schema, err := capability.DecodeSchema(m.Capability)
	if err != nil {
		writeJSON(w, http.StatusOK, probeResult{DryRun: dryRun, Error: "capability 解析失败: " + err.Error()})
		return
	}

	in := adapter.SubmitInput{
		TaskID:         "probe-" + m.ID,
		Provider:       provider,
		Model:          m,
		UpstreamModel:  m.UpstreamModel,
		Prompt:         "probe",
		Params:         defaultParams(schema),
		Credential:     os.Getenv(provider.CredentialRef),
		IdempotencyKey: "probe-" + m.ID,
	}

	if dryRun {
		writeJSON(w, http.StatusOK, probeResult{
			OK:              true,
			DryRun:          true,
			RenderedRequest: m.RequestMapping,
		})
		return
	}
	if in.Credential == "" {
		writeJSON(w, http.StatusOK, probeResult{
			DryRun: false,
			Error:  "环境变量 " + provider.CredentialRef + " 未设置，无法发出真实请求",
		})
		return
	}

	start := s.now()
	res := probeResult{DryRun: false}
	name := adapter.DriverName(m)
	sync, hasSync := s.deps.Drivers.Sync(name)
	// 同步还是异步由 adapter.IsAsync 判，不能"查得到异步就走异步"：
	// 同一个名字下可能同时挂着两个驱动（mock 即如此）。
	if adapter.IsAsync(s.deps.Drivers, m) {
		async, _ := s.deps.Drivers.Async(name)
		out, err := async.Submit(r.Context(), in)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
			res.RenderedRequest = map[string]any{"upstream_ref": out.UpstreamRef, "status": out.Status}
		}
	} else if hasSync {
		out, err := sync.Invoke(r.Context(), in)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
			res.RenderedRequest = map[string]any{"artifacts": len(out.Inline)}
		}
	} else {
		res.Error = "没有名为 " + name + " 的 driver"
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, res)
}

// defaultParams 取每个参数的默认值，凑出一组"最小可用参数"。
func defaultParams(schema capability.ModelCapabilitySchema) map[string]any {
	out := map[string]any{}
	for _, p := range schema.Params {
		if p.Default != nil {
			out[p.Key] = p.Default
		}
	}
	return out
}

// ── Skill ───────────────────────────────────────────────────────

type skillUpsert struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	SystemPrompt    string         `json:"system_prompt"`
	SystemPromptRef string         `json:"system_prompt_ref"`
	DefaultModelID  *string        `json:"default_model_id"`
	DefaultParams   map[string]any `json:"default_params"`
	Enabled         *bool          `json:"enabled"`
	Order           int            `json:"order"`
}

func (s *server) handleAdminListSkills(w http.ResponseWriter, r *http.Request) {
	items, err := s.deps.Skill.ListAll(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if items == nil {
		items = []domain.Skill{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": items})
}

func (s *server) handleAdminCreateSkill(w http.ResponseWriter, r *http.Request) {
	s.upsertSkill(w, r, "", http.StatusCreated)
}

func (s *server) handleAdminUpdateSkill(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "skillId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.upsertSkill(w, r, id, http.StatusOK)
}

func (s *server) upsertSkill(w http.ResponseWriter, r *http.Request, forceID string, okStatus int) {
	var req skillUpsert
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if forceID != "" {
		req.ID = forceID
	}
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Name) == "" {
		writeError(w, r, errFields([]domain.FieldError{
			{Key: "id", Message: "id 与 name 必填"},
		}, "参数校验未通过"))
		return
	}
	out, err := s.deps.Skill.Upsert(r.Context(), domain.Skill{
		ID:              strings.TrimSpace(req.ID),
		Name:            strings.TrimSpace(req.Name),
		Description:     req.Description,
		SystemPromptRef: req.SystemPromptRef,
		SystemPrompt:    req.SystemPrompt,
		DefaultModelID:  req.DefaultModelID,
		DefaultParams:   req.DefaultParams,
		Enabled:         req.Enabled == nil || *req.Enabled,
		Order:           req.Order,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, okStatus, out)
}

func (s *server) handleAdminDeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "skillId")
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err := s.deps.Skill.Delete(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
