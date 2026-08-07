package asset

import (
	"context"

	"github.com/aigc-pool/aigc-pool/internal/domain"
)

// LineageRecorder 记录并查询资产血缘。
//
// 血缘有两半，缺一不可:
//
//	「怎么来的」 domain.AssetSource —— 模型、提示词、参数快照，随 asset 一起存
//	「从谁来的」 domain.LineageEdge —— 谁是谁的输入 / 重跑 / 拼接来源
//
// 「做同款」读前一半（把参数原样回填进生成器），
// 「单卡重跑」和血缘视图读后一半（顺着边向上追输入、向下追派生）。
// 只做其中一半，两个功能会同时废掉一个。
//
// Record 在任务成功、资产落库后写入边。三种关系分别对应:
//
//	input         这次生成用了哪些资产作为输入（inputs 里的每一件）
//	rerun_of      单卡重跑：新产物指向被替换的旧产物
//	composed_from 一键成片：合成结果指向每一个片段
//
// Graph 按方向与深度查询，深度上限 5（openapi 定的）。
// 深度必须有上限：血缘是个可能带环的有向图（A 生成 B、B 又被用来重跑 A 的同款），
// 没有深度限制的遍历会在这种图上跑不完。实现还需对已访问节点去重。
type LineageRecorder interface {
	Record(ctx context.Context, edges []domain.LineageEdge) error
	Graph(ctx context.Context, assetID string, dir domain.LineageDirection, depth int) (domain.LineageGraph, error)
}

// MaxLineageDepth 是血缘查询的深度上限，与 openapi 的 depth 参数上限一致。
const MaxLineageDepth = 5
