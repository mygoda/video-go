package httpapi

import (
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// logBadSchema 记一条坏掉的 capability 配置。
//
// 一条坏配置不该让整个模型列表 500——那会让平台对所有人整个不可用。
// 跳过它并记一笔，管理端的 probe 会把这条配置单独暴露出来。
func (s *server) logBadSchema(modelID string, err error) {
	slog.Error("模型 capability 解码失败，已从列表中跳过", "model_id", modelID, "err", err)
}

// handleListModels 下发 capability schema 数组。
//
// **这是"接一个新模型 = 后台加一条记录、前端零改动"这句话的兑现点。**
// 它不认识任何具体模型：读一批配置行，把每行的 capability 字段解码成
// schema 原样下发。加一个模型就是多一行，这里一个字都不用改。
func (s *server) handleListModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Fingerprint 由启用中的配置行算出，配置一变它就变，因此可以直接当 ETag。
	// 拿不到指纹不是致命错误——退化成不带 ETag 的普通响应，比 500 好。
	etag, err := s.deps.Models.Fingerprint(ctx)
	if err == nil && etag != "" {
		quoted := strconv.Quote(etag)
		w.Header().Set("ETag", quoted)
		// no-cache 是"每次都回源校验"，不是"不缓存"。模型池每周在变，
		// max-age 会让用户在模型上下线后拿着过期列表点半天。
		w.Header().Set("Cache-Control", "no-cache")
		if match := r.Header.Get("If-None-Match"); match == quoted || match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	enabled := true
	f := domain.ModelFilter{Enabled: &enabled}
	if m := r.URL.Query().Get("modality"); m != "" {
		switch domain.Modality(m) {
		case domain.ModalityImage, domain.ModalityVideo:
			f.Modality = domain.Modality(m)
		default:
			writeError(w, r, errInvalid("modality 只能是 image 或 video"))
			return
		}
	}

	configs, err := s.deps.Models.List(ctx, f)
	if err != nil {
		writeError(w, r, err)
		return
	}

	schemas := make([]capability.ModelCapabilitySchema, 0, len(configs))
	for _, m := range configs {
		schema, err := capability.DecodeSchema(m.Capability)
		if err != nil {
			s.logBadSchema(m.ID, err)
			continue
		}
		// id 以配置行的主键为准：capability 里的 id 是编辑时填的，
		// 两者不一致时按它下发会让前端提交一个后端查不到的 model_id。
		schema.ID = m.ID
		schemas = append(schemas, schema)
	}

	// order 是管理员排的展示顺序；同序时按名字定序，否则同一份数据
	// 两次请求可能给出不同顺序，前端的列表会莫名跳动。
	sort.SliceStable(schemas, func(i, j int) bool {
		if schemas[i].Order != schemas[j].Order {
			return schemas[i].Order < schemas[j].Order
		}
		return schemas[i].Name < schemas[j].Name
	})

	writeJSON(w, http.StatusOK, map[string]any{"models": schemas})
}

func (s *server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := s.deps.Skill.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	if skills == nil {
		skills = []domain.Skill{}
	}
	sort.SliceStable(skills, func(i, j int) bool { return skills[i].Order < skills[j].Order })
	writeJSON(w, http.StatusOK, map[string]any{"skills": skills})
}
