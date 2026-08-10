// 画布创作链路里那几次同步的 chat 调用的共用部分：选模型、打一次上游、
// 把回复还原成文本、记一条可核对的摘要。
//
// # 为什么这几步是同步的，不走任务链路
//
// 剧本和分镜的产物都是**文字**，chat 族本身就是一次 HTTP 往返拿结果（见
// domain.ProtocolFamily 的注释）。为它建 task 行、冻结积分、走 worker、
// 再从 SSE 推回来，只是把一次几秒的调用包装成一条永远不会失败得更少的
// 异步链路，还要用户盯着转圈。因此这两步直接调 driver、直接落卡片。
//
// 出片是另一回事：一段 4 秒的视频要在上游跑一分多钟，它必须走完整任务链路
// （落库、冻结、worker、失败退款、SSE）。但出片**不在这两步里发生**——
// 画布上先有剧本卡、再有镜头卡，用户看过改过之后才轮到花钱，见 script.go。
//
// # 为什么不写死模型
//
// 走 ModelConfig：优先取配置里的默认模型（AIGC_STORYBOARD_MODEL），没配就取
// 第一个启用的 chat 族模型。不写死 id，是因为"改配置不重启不发版"正是
// models 表存在的理由。
package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// chatTrace 是一次 chat 调用的摘要，用于日志与排查。
// **不含凭证，也不含完整响应体**——只留够回答"这次是不是真的打到上游了"。
type chatTrace struct {
	ModelID       string
	UpstreamModel string
	ProviderID    string
	PromptChars   int
	ReplyChars    int
	LatencyMS     int64
}

// chatModel 选出画布链路这次要用的 chat 模型。
//
// 配置里指定了就用指定的那个（并检查它确实是 chat 族——配错时明确报错，
// 好过拿一个 video 模型去发 chat 请求然后在上游那边失败）。
func (s *server) chatModel(ctx context.Context) (domain.ModelConfig, error) {
	if id := strings.TrimSpace(s.deps.Config.StoryboardModelID); id != "" {
		m, err := s.deps.Models.Get(ctx, id)
		if err != nil {
			return domain.ModelConfig{}, err
		}
		if !m.Enabled {
			return domain.ModelConfig{}, errInvalid("配置的画布 chat 模型 %s 当前已禁用", id)
		}
		if m.Family != domain.FamilyChat {
			return domain.ModelConfig{}, errInvalid(
				"配置的画布 chat 模型 %s 的 protocol_family 是 %s，这一步需要 chat", id, m.Family)
		}
		return m, nil
	}

	enabled := true
	models, err := s.deps.Models.List(ctx, domain.ModelFilter{Enabled: &enabled})
	if err != nil {
		return domain.ModelConfig{}, err
	}
	for _, m := range models {
		if m.Family == domain.FamilyChat {
			return m, nil
		}
	}
	return domain.ModelConfig{}, &domain.Error{
		Code: domain.CodeInvalidParam,
		Message: "没有可用的 chat 模型；" +
			"请在管理后台启用一个 protocol_family=chat 的模型，或设置 AIGC_STORYBOARD_MODEL",
	}
}

// chatOnce 打一次 chat 上游，返回回复正文。
//
// step 只用来给这次调用的 id 和日志打标（"script" / "storyboard"），
// 好让日志里能分出是哪一步在调，而不是一堆分不清来源的 chat 调用。
func (s *server) chatOnce(ctx context.Context, step, prompt string) (string, chatTrace, error) {
	model, err := s.chatModel(ctx)
	if err != nil {
		return "", chatTrace{}, err
	}
	provider, err := s.deps.Providers.Get(ctx, model.ProviderID)
	if err != nil {
		return "", chatTrace{}, err
	}
	if !provider.Enabled {
		return "", chatTrace{}, errInvalid("供应商 %s 当前已禁用", provider.Name)
	}
	credential := os.Getenv(provider.CredentialRef)
	if strings.TrimSpace(credential) == "" {
		return "", chatTrace{}, errInvalid(
			"环境变量 %s 未设置，无法调用模型 %s", provider.CredentialRef, model.ID)
	}

	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		return "", chatTrace{}, errInternal(err)
	}

	in := adapter.SubmitInput{
		TaskID:         step + "-" + uid.Token(8),
		Provider:       provider,
		Model:          model,
		UpstreamModel:  model.UpstreamModel,
		Prompt:         prompt,
		Params:         defaultParams(schema),
		Credential:     credential,
		IdempotencyKey: step + "-" + uid.Token(8),
	}

	driver, err := adapter.ResolveSync(s.deps.Drivers, model)
	if err != nil {
		return "", chatTrace{}, errInternal(err)
	}

	// 超时取供应商配置，与 executor.invoke 同一套规则：一个卡住的上游
	// 不该把 HTTP handler 一起挂住。
	cctx := ctx
	if provider.TimeoutMS > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, time.Duration(provider.TimeoutMS)*time.Millisecond)
		defer cancel()
	}

	start := time.Now()
	out, err := driver.Invoke(cctx, in)
	trace := chatTrace{
		ModelID:       model.ID,
		UpstreamModel: model.UpstreamModel,
		ProviderID:    provider.ID,
		PromptChars:   len([]rune(prompt)),
		LatencyMS:     time.Since(start).Milliseconds(),
	}
	if err != nil {
		// driver 返回的已经是带分类码的 *domain.Error，原样上抛即可：
		// 包一层 internal error 会把 upstream_rate_limited 这类可重试信息抹掉。
		return "", trace, err
	}

	reply, err := chatReplyText(out)
	if err != nil {
		return "", trace, err
	}
	trace.ReplyChars = len([]rune(reply))
	return reply, trace, nil
}

// chatReplyText 把 driver 回来的产物还原成文本。
//
// chat 族的产物是 KindBase64 + text/plain（见 openaicompat.chatArtifacts），
// 这里按那个形状解，不认识的形状明确报错而不是当空字符串继续走——
// 静默的空回复会变成"画布上什么也没多出来"。
func chatReplyText(out adapter.SubmitResult) (string, error) {
	for _, a := range out.Inline {
		if a.Type != domain.AssetTypeText {
			continue
		}
		switch a.Kind {
		case adapter.KindBase64:
			raw, err := base64.StdEncoding.DecodeString(a.Base64)
			if err != nil {
				return "", errInternal(fmt.Errorf("模型回复的 base64 解码失败: %w", err))
			}
			if text := strings.TrimSpace(string(raw)); text != "" {
				return text, nil
			}
		case adapter.KindURL:
			return "", errInvalid("模型返回的是 URL 形态的文本产物，暂不支持")
		}
	}
	return "", &domain.Error{
		Code:      domain.CodeInternal,
		Message:   "模型返回成功但没有可用的文本内容",
		Retryable: true,
	}
}

// logChatCall 记一条可核对的调用摘要。
//
// 只记步骤、模型、耗时、字数——够回答"这次到底有没有打到上游、上游说了
// 多少话"，又不会把用户的剧本原文写进日志。
func logChatCall(step, projectID string, t chatTrace) {
	slog.Info("画布 chat 调用完成",
		"step", step,
		"project_id", projectID,
		"model_id", t.ModelID,
		"upstream_model", t.UpstreamModel,
		"provider_id", t.ProviderID,
		"prompt_chars", t.PromptChars,
		"reply_chars", t.ReplyChars,
		"latency_ms", t.LatencyMS)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
