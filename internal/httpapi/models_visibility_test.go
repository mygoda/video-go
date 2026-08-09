package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// 本文件守的是一条产品线，不是一个函数：**用户端目录里不许出现 mock 驱动
// 与 QA 夹具。**
//
// 2026-08-09 的事故形态是：qa-fail-probe（一个设计上必然失败的探针）和
// mock-image-1（出占位图的假驱动）一起摆在创作页的模型下拉里，用户点了就是
// 一次必失败或一张假图。根因是当时只有 enabled 一个维度，而 QA 需要这些行
// 保持可调用，于是没人敢把它们停用。
//
// 因此这条测试断的是 GET /api/models 的**响应体**，不是 handler 有没有传对
// 参数——传参正确但仓储忽略过滤，用户看到的仍然是假模型。

// catalogRepo 是一个按 domain.ModelFilter 逐字段过滤的内存模型仓储。
//
// 过滤逻辑与 SQL WHERE 一一对应（等值匹配、零值表示不过滤），这样这条测试
// 才在断"用户看到了什么"，而不是在断"handler 构造了什么 filter"。
type catalogRepo struct {
	store.ModelRepo // 未覆盖的方法一律 panic：本测试不该用到它们
	all             []domain.ModelConfig
}

func (r *catalogRepo) List(_ context.Context, f domain.ModelFilter) ([]domain.ModelConfig, error) {
	var out []domain.ModelConfig
	for _, m := range r.all {
		if f.Modality != "" && f.Modality != modalityOf(m) {
			continue
		}
		if f.Enabled != nil && *f.Enabled != m.Enabled {
			continue
		}
		if f.Visibility != "" && f.Visibility != m.Visibility {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *catalogRepo) Fingerprint(context.Context) (string, error) { return "", nil }

func modalityOf(m domain.ModelConfig) domain.Modality {
	var c struct {
		Modality domain.Modality `json:"modality"`
	}
	raw, _ := json.Marshal(m.Capability)
	_ = json.Unmarshal(raw, &c)
	return c.Modality
}

type stubUsers struct {
	store.UserRepo
	u domain.User
}

func (s stubUsers) GetByID(context.Context, string) (domain.User, error) { return s.u, nil }
func (s stubUsers) TouchLastActive(context.Context, string, time.Time) error {
	return nil
}

type stubTokens struct{ role domain.Role }

func (stubTokens) Issue(string, domain.Role, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (s stubTokens) Verify(string) (string, domain.Role, error) {
	return "user-1", s.role, nil
}

// qaProviderID 是那台"永远连不上"的 QA 供应商，qa-fail-probe 挂在它下面。
const qaProviderID = "019fdeb7-5511-7fbe-b4b8-1a0622a9f1bb"

func capabilityJSON(id, name string, modality domain.Modality) map[string]any {
	return map[string]any{
		"id": id, "name": name, "vendor": "t", "modality": string(modality),
		"enabled": true, "order": 10,
		"inputs": []any{}, "params": []any{},
		"pricing": map[string]any{"currency": "credit", "base": 1, "rounding": "ceil"},
	}
}

// catalogFixture 复刻线上模型池的形态：三条真实上游 + 四条必须藏起来的行。
func catalogFixture() []domain.ModelConfig {
	mockVP := domain.VideoProtocolMock
	predVP := domain.VideoProtocolPredictions
	return []domain.ModelConfig{
		{
			ID: "gpugeek-seedream-4-5", ProviderID: "gpugeek", Family: domain.FamilyPredictions,
			Capability: capabilityJSON("gpugeek-seedream-4-5", "Seedream 4.5", domain.ModalityImage),
			Enabled:    true, Visibility: domain.VisibilityPublic,
		},
		{
			ID: "gpugeek-seedance-2-0", ProviderID: "gpugeek", Family: domain.FamilyVideo,
			VideoProtocol: &predVP,
			Capability:    capabilityJSON("gpugeek-seedance-2-0", "Seedance 2.0", domain.ModalityVideo),
			Enabled:       true, Visibility: domain.VisibilityPublic,
		},
		{
			ID: "mock-image-1", ProviderID: "mock", Family: domain.FamilyMock,
			Capability: capabilityJSON("mock-image-1", "假图", domain.ModalityImage),
			Enabled:    true, Visibility: domain.VisibilityInternal,
		},
		{
			ID: "mock-video-1", ProviderID: "mock", Family: domain.FamilyVideo,
			VideoProtocol: &mockVP,
			Capability:    capabilityJSON("mock-video-1", "假视频", domain.ModalityVideo),
			Enabled:       true, Visibility: domain.VisibilityInternal,
		},
		{
			ID: "qa-schema-probe", ProviderID: "mock", Family: domain.FamilyMock,
			Capability: capabilityJSON("qa-schema-probe", "QA schema 夹具", domain.ModalityImage),
			Enabled:    true, Visibility: domain.VisibilityInternal,
		},
		{
			ID: "qa-fail-probe", ProviderID: qaProviderID, Family: domain.FamilyImages,
			Capability: capabilityJSON("qa-fail-probe", "QA 失败注入夹具", domain.ModalityImage),
			Enabled:    true, Visibility: domain.VisibilityInternal,
		},
		{
			ID: "local-compose-1", ProviderID: "local", Family: domain.FamilyCompose,
			Capability: capabilityJSON("local-compose-1", "分镜合成（本地）", domain.ModalityVideo),
			Enabled:    true, Visibility: domain.VisibilityInternal,
		},
	}
}

func userCatalogHandler(t *testing.T, models []domain.ModelConfig) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux, Deps{
		Tokens: stubTokens{role: domain.RoleUser},
		Users: stubUsers{u: domain.User{
			ID: "user-1", Username: "u", Role: domain.RoleUser, Status: domain.UserStatusActive,
		}},
		Models: &catalogRepo{all: models},
	})
	return mux
}

// listAsUser 以普通用户身份请求 GET /api/models，返回下发的 capability 列表。
func listAsUser(t *testing.T, h http.Handler, query string) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/models"+query, nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models%s = %d, body=%s", query, rec.Code, rec.Body.String())
	}
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v; body=%s", err, rec.Body.String())
	}
	return body.Models
}

// TestUserCatalogExcludesMockAndQAFixtures 是本 issue 最重要的产出。
//
// 断言两件事，缺一不可：
//  1. 用户拿到的目录里没有任何 mock 驱动或 QA 夹具（按 id 逐条点名，
//     用 id 而不是 protocol_family 是因为响应里根本没有 protocol_family——
//     用户看到的就是这份 capability，能骗到用户的也只有这份）；
//  2. 真实上游模型仍在。少了这半条，把 handler 改成永远返回空数组
//     也能让第 1 条通过。
func TestUserCatalogExcludesMockAndQAFixtures(t *testing.T) {
	h := userCatalogHandler(t, catalogFixture())

	forbidden := map[string]string{
		"mock-image-1":    "mock 驱动，出的是占位图",
		"mock-video-1":    "mock 驱动，出的是占位视频",
		"qa-schema-probe": "QA 的畸形 schema 夹具",
		"qa-fail-probe":   "QA 的失败注入夹具，挂在不可达供应商上",
		"local-compose-1": "需要 >=2 个片段输入，创作页喂不出来，必失败",
	}

	for _, query := range []string{"", "?modality=image", "?modality=video"} {
		got := listAsUser(t, h, query)
		seen := map[string]bool{}
		for _, m := range got {
			id, _ := m["id"].(string)
			seen[id] = true
			if why, bad := forbidden[id]; bad {
				t.Errorf("GET /api/models%s 把 %s 下发给了用户端（%s）", query, id, why)
			}
		}
		if query == "" {
			for _, id := range []string{"gpugeek-seedream-4-5", "gpugeek-seedance-2-0"} {
				if !seen[id] {
					t.Errorf("GET /api/models 漏掉了真实上游模型 %s", id)
				}
			}
		}
	}
}

// TestUserCatalogHidesDisabledModels 守的是另一个维度：visibility 与 enabled
// 正交，加了前者不能把后者顶掉。
func TestUserCatalogHidesDisabledModels(t *testing.T) {
	models := append(catalogFixture(), domain.ModelConfig{
		ID: "gpugeek-seedream-5-0-lite", ProviderID: "gpugeek", Family: domain.FamilyPredictions,
		Capability: capabilityJSON("gpugeek-seedream-5-0-lite", "下架中", domain.ModalityImage),
		Enabled:    false, Visibility: domain.VisibilityPublic,
	})
	for _, m := range listAsUser(t, userCatalogHandler(t, models), "") {
		if m["id"] == "gpugeek-seedream-5-0-lite" {
			t.Fatal("已停用的模型仍然出现在用户端目录里")
		}
	}
}

// TestUserCatalogRejectsUnknownModality 守住 text 进入枚举后 modality 参数
// 仍然是白名单：它直接进 SQL 的 WHERE，放开等于把过滤条件交给调用方拼。
func TestUserCatalogRejectsUnknownModality(t *testing.T) {
	h := userCatalogHandler(t, catalogFixture())
	req := httptest.NewRequest(http.MethodGet, "/api/models?modality=audio", nil)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("modality=audio 应当 400，实得 %d: %s", rec.Code, rec.Body.String())
	}
}
