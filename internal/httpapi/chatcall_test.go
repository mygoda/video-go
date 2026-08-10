package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aigc-pool/aigc-pool/internal/config"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// 本文件守的是选模型这一步，重点是那道**越权闸**：用户在请求体里点名的
// model_id 必须是 public 的。
//
// GET /api/models 只下发 public 模型，但那只是"没告诉用户"——internal 模型的
// id 全写在迁移文件里，也出现在管理端和日志里。少了这道闸，任何人把一个
// internal 的 id 贴进请求体就能调到它，000008 建立的目录可见性就退化成一层
// 前端提示。因此这里断的是"点名一个 internal 模型会不会被拒"，而不是
// "handler 有没有读到 model_id"。

// chatModelRepo 是一个只实现 Get / List 的内存模型仓储。
type chatModelRepo struct {
	store.ModelRepo // 未覆盖的方法一律 panic：本测试不该用到它们
	all             []domain.ModelConfig
}

func (r *chatModelRepo) Get(_ context.Context, id string) (domain.ModelConfig, error) {
	for _, m := range r.all {
		if m.ID == id {
			return m, nil
		}
	}
	return domain.ModelConfig{}, errNotFound("model")
}

func (r *chatModelRepo) List(_ context.Context, f domain.ModelFilter) ([]domain.ModelConfig, error) {
	var out []domain.ModelConfig
	for _, m := range r.all {
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

// chatModelFixture 复刻目录里与写剧本有关的那几行。
func chatModelFixture() []domain.ModelConfig {
	return []domain.ModelConfig{
		{
			ID: "gpugeek-deepseek-v4-pro", Family: domain.FamilyChat,
			Enabled: true, Visibility: domain.VisibilityPublic,
		},
		{
			ID: "gpugeek-qwen3-vl-plus", Family: domain.FamilyChat,
			Enabled: true, Visibility: domain.VisibilityInternal,
		},
		{
			ID: "gpugeek-qwen3-max", Family: domain.FamilyChat,
			Enabled: false, Visibility: domain.VisibilityPublic,
		},
		{
			ID: "gpugeek-seedance-2-0", Family: domain.FamilyVideo,
			Enabled: true, Visibility: domain.VisibilityPublic,
		},
	}
}

func chatModelServer(storyboardModel string) *server {
	return &server{deps: Deps{
		Config: config.Config{StoryboardModelID: storyboardModel},
		Models: &chatModelRepo{all: chatModelFixture()},
	}}
}

// TestUserChatModelRejectsUnselectableModels 是本文件最重要的断言：
// 用户点名的模型必须启用、必须是 chat 族、且必须在用户目录里。
func TestUserChatModelRejectsUnselectableModels(t *testing.T) {
	cases := []struct {
		name    string
		modelID string
		wantErr string
	}{
		{"internal 模型不许被点名", "gpugeek-qwen3-vl-plus", "不在可选的模型目录里"},
		{"停用的模型不许被点名", "gpugeek-qwen3-max", "当前已禁用"},
		{"非 chat 族不许写剧本", "gpugeek-seedance-2-0", "需要 chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := chatModelServer("").chatModel(context.Background(), tc.modelID)
			if err == nil {
				t.Fatalf("点名 %s 本该被拒，却通过了", tc.modelID)
			}
			var de *domain.Error
			if !errors.As(err, &de) {
				t.Fatalf("期望 *domain.Error，实得 %T: %v", err, err)
			}
			if de.Code != domain.CodeInvalidParam {
				t.Errorf("错误码 = %s，期望 %s", de.Code, domain.CodeInvalidParam)
			}
			// 报错要指名道姓落在 model_id 上：回一句笼统的"参数不对"会让前端
			// 没法把红框标在模型下拉上。
			if len(de.FieldErrors) != 1 || de.FieldErrors[0].Key != "model_id" {
				t.Fatalf("期望一条 model_id 的字段错误，实得 %+v", de.FieldErrors)
			}
			if !strings.Contains(de.FieldErrors[0].Message, tc.wantErr) {
				t.Errorf("报错文案 = %q，期望包含 %q", de.FieldErrors[0].Message, tc.wantErr)
			}
		})
	}
}

// TestChatModelPrecedence 钉住三级优先级，重点是**不传 model_id 时行为不变**：
// 这个字段是后加的，它一旦改动了默认路径，画布上所有既有的写剧本请求都会跟着变。
func TestChatModelPrecedence(t *testing.T) {
	t.Run("点名优先于配置", func(t *testing.T) {
		m, err := chatModelServer("gpugeek-qwen3-vl-plus").
			chatModel(context.Background(), "gpugeek-deepseek-v4-pro")
		if err != nil {
			t.Fatalf("选模型失败: %v", err)
		}
		if m.ID != "gpugeek-deepseek-v4-pro" {
			t.Errorf("选中 %s，期望点名的 gpugeek-deepseek-v4-pro", m.ID)
		}
	})

	// 配置项故意不查 visibility：那是运维在服务端设的默认后端，而 visibility
	// 管的是"摆不摆进用户下拉"，不是"能不能调用"（见 000008）。这条 internal
	// 模型作为配置默认值必须仍然选得中。
	t.Run("不点名时走配置，配置不受 visibility 限制", func(t *testing.T) {
		m, err := chatModelServer("gpugeek-qwen3-vl-plus").chatModel(context.Background(), "")
		if err != nil {
			t.Fatalf("选模型失败: %v", err)
		}
		if m.ID != "gpugeek-qwen3-vl-plus" {
			t.Errorf("选中 %s，期望配置里的 gpugeek-qwen3-vl-plus", m.ID)
		}
	})

	t.Run("既不点名也没配置时取第一个启用的 chat 模型", func(t *testing.T) {
		m, err := chatModelServer("").chatModel(context.Background(), "")
		if err != nil {
			t.Fatalf("选模型失败: %v", err)
		}
		if m.ID != "gpugeek-deepseek-v4-pro" {
			t.Errorf("选中 %s，期望第一个启用的 chat 模型", m.ID)
		}
	})

	// 空白串等同于没传：前端把下拉清空发上来的是 ""，若它被当成一次点名，
	// 用户会收到一个"模型 不在可选的模型目录里"的报错。
	t.Run("空白 model_id 等同于没传", func(t *testing.T) {
		m, err := chatModelServer("gpugeek-qwen3-vl-plus").chatModel(context.Background(), "   ")
		if err != nil {
			t.Fatalf("选模型失败: %v", err)
		}
		if m.ID != "gpugeek-qwen3-vl-plus" {
			t.Errorf("选中 %s，期望回落到配置", m.ID)
		}
	})
}
