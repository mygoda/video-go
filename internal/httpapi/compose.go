// 画布的一键合成：把用户选中的若干张卡片按顺序合成一件新产物。
//
// # 它产出的是一条真任务，不是一个假 id
//
// 合成走的是与生成**完全相同**的那条链路：tasks 表落一行、queued → running
// → succeeded 的状态机、失败全额退款、产物转存进资产库、血缘边指回源卡片、
// SSE 推状态、任务监控里能查到。之所以不在 handler 里就地拼一份东西直接
// 返回，正是因为前端要的是一个「可查的任务」——501 与假 id 是同一种病的
// 两种症状：前端拿不到能追踪的东西。
//
// # 拼接本身在 driver 里，本文件不碰字节
//
// 底下的 compose driver 用 ffmpeg 把片段首尾相接成一条 MP4。本文件只负责
// 把「用户选中的这几张卡片」翻译成「按顺序排好的这几件资产」，以及提交任务
// 前的那一串校验与计价。换一种拼接实现（比如接一个上游剪辑服务）时，
// 换掉的只是那个 driver，本文件一行不改。
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/aigc-pool/aigc-pool/internal/adapter/compose"
	"github.com/aigc-pool/aigc-pool/internal/capability"
	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/stream"
	"github.com/aigc-pool/aigc-pool/internal/uid"
)

// composeModel 选出这次合成要用的模型。
//
// 与 storyboardModel 同一套规则：配了就用配的（并检查它确实是 compose 族），
// 没配就取第一个启用的 compose 族模型。写死一个 id 意味着"换成真拼接实现"
// 要改代码发版，而那正是 models 表存在的理由。
func (s *server) composeModel(ctx context.Context) (domain.ModelConfig, error) {
	if id := strings.TrimSpace(s.deps.Config.ComposeModelID); id != "" {
		m, err := s.deps.Models.Get(ctx, id)
		if err != nil {
			return domain.ModelConfig{}, err
		}
		if !m.Enabled {
			return domain.ModelConfig{}, errInvalid("配置的合成模型 %s 当前已禁用", id)
		}
		if m.Family != domain.FamilyCompose {
			return domain.ModelConfig{}, errInvalid(
				"配置的合成模型 %s 的 protocol_family 是 %s，合成需要 compose", id, m.Family)
		}
		return m, nil
	}

	enabled := true
	models, err := s.deps.Models.List(ctx, domain.ModelFilter{Enabled: &enabled})
	if err != nil {
		return domain.ModelConfig{}, err
	}
	for _, m := range models {
		if m.Family == domain.FamilyCompose {
			return m, nil
		}
	}
	return domain.ModelConfig{}, &domain.Error{
		Code: domain.CodeInvalidParam,
		Message: "没有可用的 compose 模型，无法合成；" +
			"请在管理后台启用一个 protocol_family=compose 的模型，或设置 AIGC_COMPOSE_MODEL",
	}
}

// handleCanvasCompose 把一组有序的卡片合成一件新产物。
//
// 请求体里的 card_ids 是**有序**的，顺序即片段顺序——不排序、不去歧义，
// 用户在画布上点选的先后就是成片的先后。
func (s *server) handleCanvasCompose(w http.ResponseWriter, r *http.Request) {
	id, err := identity(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	p, err := s.ownedProject(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	var req struct {
		CardIDs     []string `json:"card_ids"`
		Title       string   `json:"title"`
		ClientToken string   `json:"client_token"`
		// Mute 去掉成片的音轨。人声本身是出片那一步决定的（台词进不进
		// prompt），这里是给已经出好的片子一条不必重出的静音路径。
		Mute bool `json:"mute"`
		// Subtitles 决定要不要挂一条软字幕轨，正文取各镜的台词字段。
		Subtitles bool `json:"subtitles"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.ClientToken == "" {
		writeError(w, r, errFields(
			[]domain.FieldError{{Key: "client_token", Message: "必填，用于幂等提交"}}, "参数校验未通过"))
		return
	}
	ctx := r.Context()

	// 幂等先于一切校验：断网重发拿回同一条任务，而不是又合成一次。
	if existing, ok, err := s.deps.Tasks.GetByClientToken(ctx, id.UserID, req.ClientToken); err != nil {
		writeError(w, r, err)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, taskAccepted(existing))
		return
	}

	snap, err := s.deps.Canvas.Snapshot(ctx, p.ID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	assetIDs, err := composeSegments(snap.Cards, req.CardIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 卡片属于本项目、项目属于本人，但资产是另一张表——卡片上那个 asset_id
	// 是客户端写进来的，不校验归属就等于"改一下卡片就能把别人的产物拼进来"。
	assets, err := s.ownedAssets(ctx, id.UserID, assetIDs)
	if err != nil {
		writeError(w, r, err)
		return
	}

	params, err := composeParams(req.Mute, req.Subtitles, snap.Cards, req.CardIDs, assets)
	if err != nil {
		writeError(w, r, err)
		return
	}

	model, err := s.composeModel(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	schema, err := capability.DecodeSchema(model.Capability)
	if err != nil {
		writeError(w, r, errInternal(err))
		return
	}

	inputs := map[string][]string{compose.SlotSegments: assetIDs}
	sub := capability.Submission{ModelID: model.ID, Prompt: req.Title, Inputs: inputs, Params: params}
	if fields := s.deps.Validator.Validate(schema, sub); len(fields) > 0 {
		writeError(w, r, errFields(fields, "参数校验未通过"))
		return
	}

	cost, err := s.deps.Pricer.Estimate(schema.Pricing, capability.EvalContext{Params: params, Inputs: inputs})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if bal, err := s.deps.Ledger.Balance(ctx, id.UserID); err == nil && bal.Available < cost {
		writeError(w, r, &domain.Error{
			Code:    domain.CodeInsufficientCredit,
			Message: "积分不足：需要 " + itoa(cost) + "，可用 " + itoa(bal.Available),
		})
		return
	}

	// CanvasID 让这条任务出现在该画布的任务列表里；不带的话前端按 canvas_id
	// 过滤时看不到自己刚提交的合成。
	canvasID := p.ID
	task, err := s.deps.Tasks.Create(ctx, domain.Task{
		ID:            uid.New(),
		UserID:        id.UserID,
		ModelID:       model.ID,
		ProviderID:    model.ProviderID,
		Status:        domain.TaskStatusQueued,
		Prompt:        req.Title,
		Params:        params,
		Inputs:        inputs,
		EstimatedCost: cost,
		CanvasID:      &canvasID,
		ClientToken:   req.ClientToken,
		CreatedAt:     s.now(),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	// 与 handleCreateTask 同一套补偿：冻结失败就把任务判失败并标注未扣费，
	// expect=queued 保证 worker 已经捞走时不会覆盖它的状态。
	if _, err := s.deps.Ledger.Hold(ctx, id.UserID, task.ID, cost); err != nil {
		terr := taskErrorFrom(err)
		_ = s.deps.Tasks.UpdateStatus(detachedContext(), task.ID,
			domain.TaskStatusQueued, domain.TaskStatusFailed, &terr)
		writeError(w, r, err)
		return
	}

	if eta := schema.ETA.P50Seconds; eta > 0 {
		task.ETASeconds = &eta
	}
	s.publish(ctx, stream.TaskUpdated(id.UserID, task))
	s.publishBalance(ctx, id.UserID)

	writeJSON(w, http.StatusCreated, taskAccepted(task))
}

// composeSegments 按 ids 的顺序取出每张卡片的产物资产 id。
//
// 三种情况分别报清楚：卡片不在这张画布上、卡片还没有产出、以及连两段都
// 凑不齐。合成失败最常见的原因就是"选了一张还在生成中的卡片"，把出错的
// 卡片 id 列出来，用户才知道该取消勾选哪一张——回一句"参数错误"等于让他挨个试。
func composeSegments(cards []domain.Card, ids []string) ([]string, error) {
	if len(ids) < 2 {
		return nil, errFields([]domain.FieldError{
			{Key: "card_ids", Message: "至少要选 2 张卡片"},
		}, "参数校验未通过")
	}

	byID := make(map[string]domain.Card, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}

	var missing, empty []string
	out := make([]string, 0, len(ids))
	for _, cardID := range ids {
		c, ok := byID[cardID]
		if !ok {
			missing = append(missing, cardID)
			continue
		}
		if c.AssetID == nil || *c.AssetID == "" {
			empty = append(empty, cardID)
			continue
		}
		out = append(out, *c.AssetID)
	}

	var fields []domain.FieldError
	if len(missing) > 0 {
		fields = append(fields, domain.FieldError{
			Key:     "card_ids",
			Message: "这些卡片不在本画布上：" + strings.Join(missing, ", "),
		})
	}
	if len(empty) > 0 {
		fields = append(fields, domain.FieldError{
			Key:     "card_ids",
			Message: "这些卡片还没有产出，无法参与合成：" + strings.Join(empty, ", "),
		})
	}
	if len(fields) > 0 {
		return nil, errFields(fields, "参数校验未通过")
	}
	return out, nil
}

// assertOwnsAssets 校验每件参与合成的资产都属于调用方，并把它们按序返回。
//
// 与 assertInputRefs 同理：不查这一步，猜到（或从别处抄到）一个 asset_id
// 写进卡片就能把别人的产物拼进自己的成片。找不到与不属于本人回同一句话，
// 不泄露"这个 id 存在但不是你的"。
//
// 顺带把资产本身交出来，是因为字幕的时间轴要按各段的 duration_ms 累加，
// 而这里已经逐件取过一遍了——为同一批 id 再查一轮库只是让合成多几个来回。
func (s *server) ownedAssets(ctx context.Context, userID string, assetIDs []string) ([]domain.Asset, error) {
	out := make([]domain.Asset, 0, len(assetIDs))
	for _, assetID := range assetIDs {
		a, err := s.deps.Assets.Get(ctx, assetID)
		if err != nil {
			var de *domain.Error
			if errors.As(err, &de) && de.Code == domain.CodeNotFound {
				return nil, errFields([]domain.FieldError{
					{Key: "card_ids", Message: "产物 " + assetID + " 不存在或已删除"},
				}, "参数校验未通过")
			}
			return nil, err
		}
		if a.UserID != userID {
			return nil, errFields([]domain.FieldError{
				{Key: "card_ids", Message: "产物 " + assetID + " 不存在或已删除"},
			}, "参数校验未通过")
		}
		out = append(out, a)
	}
	return out, nil
}

// composeParams 把两个开关翻译成 compose driver 认得的 params。
//
// **两个键一律都写，哪怕都是默认值。** 校验器要求目录里声明过、且当前可见的
// 每一个参数都出现在提交里（validateParams 的 !present 分支），少一个就是
// 400「…为必填」。省掉默认值看着干净，代价是「什么都不选的合成」根本提不上去。
func composeParams(mute, subtitles bool, cards []domain.Card, cardIDs []string, assets []domain.Asset) (map[string]any, error) {
	srt := ""
	if subtitles {
		// 一段台词都没写时留空串：一条没有任何条目的字幕轨在播放器的字幕
		// 菜单里照样占一项，用户点开发现全程没字，只会以为字幕功能坏了。
		s, err := composeSubtitles(cards, cardIDs, assets)
		if err != nil {
			return nil, err
		}
		srt = s
	}
	return map[string]any{
		compose.ParamMute:        mute,
		compose.ParamSubtitleSRT: srt,
	}, nil
}

// composeSubtitles 按片段顺序生成整片的 SRT。
//
// 文本逐字取镜头卡的 params.dialogue，**不做任何语音转写**：台词是用户自己
// 写的，手上有原文还去转写只会引入误差。非镜头卡（用户随手拼进来的图片卡、
// 视频卡）没有台词字段，它们照样占时间轴，只是不出字。
//
// 时长取资产的 duration_ms，与 driver 落盘后探到的时长同源。图片资产在库里
// 没有时长——那一段的长度是 driver 现给的，所以这里必须取同一个常量。
// 视频资产缺时长则直接报错：那种情况下算出来的时间轴从这一段起全是错的，
// 而一条整体偏移的字幕比没有字幕更难被发现。
func composeSubtitles(cards []domain.Card, cardIDs []string, assets []domain.Asset) (string, error) {
	byID := make(map[string]domain.Card, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}

	cues := make([]compose.Cue, 0, len(cardIDs))
	for i, cardID := range cardIDs {
		card := byID[cardID]

		var dialogue string
		if card.Kind == domain.CardKindShot {
			// params 是这张卡自己的既有内容，能落库就一定解得开；真解不开时
			// 这一镜按无台词处理，不该因此把整次合成拒掉。
			if sp, err := domain.ParseShotParams(card.Params); err == nil {
				dialogue = sp.Dialogue
			}
		}

		ms, err := segmentDurationMS(assets[i])
		if err != nil {
			return "", err
		}
		cues = append(cues, compose.Cue{Text: dialogue, DurationMS: ms})
	}
	return compose.SRT(cues), nil
}

// segmentDurationMS 报告一件资产在成片里占多少毫秒。
func segmentDurationMS(a domain.Asset) (int, error) {
	if a.DurationMS != nil && *a.DurationMS > 0 {
		return *a.DurationMS, nil
	}
	if a.Type == domain.AssetTypeImage {
		return compose.StillSegmentMS, nil
	}
	return 0, errFields([]domain.FieldError{{
		Key: "subtitles",
		Message: "产物 " + a.ID + " 没有记录时长，字幕的时间轴会从它开始整体错位；" +
			"先重出这一段，或者这次先不加字幕",
	}}, "参数校验未通过")
}
