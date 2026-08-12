// 画布创作链路里那几次同步的 chat 调用的共用部分：选模型、打一次上游、
// 把回复还原成文本、记一条可核对的摘要。
//
// # 为什么这几步是同步的，不走任务链路
//
// 剧本和分镜的产物都是**文字**，chat 族本身就是一次 HTTP 往返拿结果（见
// domain.ProtocolFamily 的注释）。把它塞进 worker 再从 SSE 推回来，只是把
// 一次几秒的调用包装成一条永远不会失败得更少的异步链路，还要用户盯着转圈。
// 因此这几步直接调 driver、直接落卡片。
//
// 不走 worker 不等于不花钱：这几次调用同样烧真 token，因此同样要过计费闸，
// 只是冻结与结算都在这一次 HTTP 请求内闭环（见 chatbill.go）。
//
// 出片是另一回事：一段 4 秒的视频要在上游跑一分多钟，它必须走完整任务链路
// （落库、冻结、worker、失败退款、SSE）。但出片**不在这两步里发生**——
// 画布上先有剧本卡、再有镜头卡，用户看过改过之后才轮到花钱，见 script.go。
//
// # 为什么不写死模型
//
// 走 ModelConfig，三级优先级：这次请求点名的 model_id → 配置里的默认模型
// （AIGC_STORYBOARD_MODEL）→ 第一个启用的 chat 族模型。不写死 id，是因为
// "改配置不重启不发版"正是 models 表存在的理由；而最前面那一级让"这一版
// 剧本用哪个模型写"成为用户能做的选择，而不是运维才能改的全局设置。
package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/aigc-pool/aigc-pool/internal/adapter"
	"github.com/aigc-pool/aigc-pool/internal/asset"
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
// requested 是调用方（最终是用户）在这一次请求里点名的模型，空串表示没点名。
// 三级优先级：**这次点名 → 全局配置（AIGC_STORYBOARD_MODEL）→ 第一个启用的
// chat 模型**。点名走的校验比配置那条多一项 visibility，理由见 userChatModel。
//
// 配置里指定了就用指定的那个（并检查它确实是 chat 族——配错时明确报错，
// 好过拿一个 video 模型去发 chat 请求然后在上游那边失败）。
func (s *server) chatModel(ctx context.Context, requested string) (domain.ModelConfig, error) {
	if id := strings.TrimSpace(requested); id != "" {
		return s.userChatModel(ctx, id)
	}

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

// userChatModel 校验一个由**用户**点名的 chat 模型。
//
// 比配置那条路多一项 visibility=public 的检查，而这一项才是这个函数存在的
// 理由。目录端点（GET /api/models）只下发 public 的模型，但那只是"没告诉
// 用户"——internal 模型的 id 全在 000003 / 000004 这些迁移里写着，也会出现在
// 管理端和日志里。不在这里挡一道，任何人把一个 internal 的 id 直接贴进请求
// 体就能调到它，目录可见性就退化成了一层前端提示。
//
// 反过来，配置项（AIGC_STORYBOARD_MODEL）**故意**不查 visibility：那是运维
// 在服务端设的默认值，平台内部拿一个 internal 模型当默认后端本来就合法
// （见 000008 的注释：visibility 管的是"摆不摆进用户下拉"，不是"能不能调用"）。
// 两条路径校验不同，是因为信任来源不同。
//
// 三个失败都回 invalid_param 而不是 not_found：对用户来说"这个 model_id
// 不能用"是同一件事，而分成两种码只会让前端多写一个分支。
func (s *server) userChatModel(ctx context.Context, id string) (domain.ModelConfig, error) {
	reject := func(format string, a ...any) (domain.ModelConfig, error) {
		return domain.ModelConfig{}, errFields(
			[]domain.FieldError{{Key: "model_id", Message: fmt.Sprintf(format, a...)}},
			"指定的模型不能用于写剧本")
	}

	m, err := s.deps.Models.Get(ctx, id)
	if err != nil {
		return domain.ModelConfig{}, err
	}
	if !m.Enabled {
		return reject("模型 %s 当前已禁用", id)
	}
	if m.Family != domain.FamilyChat {
		return reject("模型 %s 的 protocol_family 是 %s，写剧本这一步需要 chat", id, m.Family)
	}
	if m.Visibility != domain.VisibilityPublic {
		return reject("模型 %s 不在可选的模型目录里", id)
	}
	return m, nil
}

// chatOnce 打一次 chat 上游，返回回复正文与**已经冻结好的那笔账**。
//
// # 返回的 bill 意味着什么
//
// 非 nil 的 bill 表示钱已经冻上了，调用方必须把它结掉：产物落到画布上之后
// 调 charge，此前的任何失败调 refund。err 非 nil 时 bill 恒为 nil——本函数
// 内部失败的那几种（上游报错、回复解析不出来）在返回前就已经自己退掉了，
// 调用方不必也不该再退一次。
//
// # 冻结点为什么在这里
//
// 选模型、查供应商、读凭证、解 schema、找 driver 这几步全都可能失败，而它们
// 一次上游都没打，失败是免费的——在它们之前冻结，只是让用户为一次没发生的
// 调用先垫一笔钱，再靠退款还回去。冻结压到 driver.Invoke 的**前一行**，
// 「余额不够时上游根本不会被调用」这条才是结构上成立的，而不是靠时序侥幸。
func (s *server) chatOnce(ctx context.Context, call chatCall) (string, chatTrace, *chatBill, error) {
	model := domain.ModelConfig{}
	if call.model != nil {
		// 服务端已选好模型（如传图强制走视觉模型），跳过选型与 visibility 校验。
		model = *call.model
	} else {
		var err error
		model, err = s.chatModel(ctx, call.modelID)
		if err != nil {
			return "", chatTrace{}, nil, err
		}
	}
	provider, err := s.deps.Providers.Get(ctx, model.ProviderID)
	if err != nil {
		return "", chatTrace{}, nil, err
	}
	if !provider.Enabled {
		return "", chatTrace{}, nil, errInvalid("供应商 %s 当前已禁用", provider.Name)
	}
	credential := os.Getenv(provider.CredentialRef)
	if strings.TrimSpace(credential) == "" {
		return "", chatTrace{}, nil, errInvalid(
			"环境变量 %s 未设置，无法调用模型 %s", provider.CredentialRef, model.ID)
	}

	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		return "", chatTrace{}, nil, errInternal(err)
	}

	driver, err := adapter.ResolveSync(s.deps.Drivers, model)
	if err != nil {
		return "", chatTrace{}, nil, errInternal(err)
	}

	in := adapter.SubmitInput{
		TaskID:         call.step + "-" + uid.Token(8),
		Provider:       provider,
		Model:          model,
		UpstreamModel:  model.UpstreamModel,
		Prompt:         call.prompt,
		Params:         defaultParams(schema),
		Inputs:         call.inputs,
		Blobs:          chatBlobReader{s.deps.Blobs},
		Credential:     credential,
		IdempotencyKey: call.step + "-" + uid.Token(8),
	}

	bill, err := s.holdChat(ctx, call, model, schema)
	if err != nil {
		return "", chatTrace{}, nil, err
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
		PromptChars:   len([]rune(call.prompt)),
		LatencyMS:     time.Since(start).Milliseconds(),
	}
	if err != nil {
		// 退款用 ctx 而不是 cctx：超时正是要退的那一类失败，
		// 拿已经到期的 cctx 去写库，退款本身会立刻被取消掉。
		bill.refund(ctx, err)
		// driver 返回的已经是带分类码的 *domain.Error，原样上抛即可：
		// 包一层 internal error 会把 upstream_rate_limited 这类可重试信息抹掉。
		return "", trace, nil, err
	}

	reply, err := chatReplyText(out)
	if err != nil {
		bill.refund(ctx, err)
		return "", trace, nil, err
	}
	trace.ReplyChars = len([]rune(reply))
	return reply, trace, bill, nil
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

// chatBlobReader 把 asset.Store 适配成 adapter.BlobReader，供内联素材（识图的
// 那张图）走 mapping 的 inline 规则读原始字节。与 executor 里那份同构，只是
// 同步 chat 链路不经 worker，得在这一层自己拿一个。
type chatBlobReader struct{ store asset.Store }

func (b chatBlobReader) ReadBlob(ctx context.Context, storageKey string) ([]byte, error) {
	rc, _, err := b.store.Get(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
