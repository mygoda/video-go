package asset

import (
	"context"
	"fmt"

	"github.com/aigc-pool/aigc-pool/internal/domain"
	"github.com/aigc-pool/aigc-pool/internal/store"
)

// LineageDeps 是 LineageRecorder 的依赖。
type LineageDeps struct {
	Assets store.AssetRepo
}

// lineageRecorder 是 LineageRecorder 的默认实现。
type lineageRecorder struct {
	assets store.AssetRepo
}

// NewLineageRecorder 组装一个血缘记录器。
func NewLineageRecorder(d LineageDeps) (LineageRecorder, error) {
	if d.Assets == nil {
		return nil, fmt.Errorf("asset: LineageDeps.Assets 不能为空")
	}
	return &lineageRecorder{assets: d.Assets}, nil
}

// Record 写入一批血缘边。
//
// 先做去重与自环剔除再落库：自环（A → A）会让 Graph 的遍历原地打转，
// 而重复边在 (child, parent, relation) 主键上会撞成一次唯一键冲突，
// 把一次本该成功的记录变成错误。两者都是调用方极易产生的形态——
// 「一次生成同时用了同一张参考图的两个槽」就会给出两条一样的 input 边。
func (r *lineageRecorder) Record(ctx context.Context, edges []domain.LineageEdge) error {
	clean := dedupeEdges(edges)
	if len(clean) == 0 {
		return nil
	}
	if err := r.assets.AddLineage(ctx, clean); err != nil {
		return fmt.Errorf("asset: 写入血缘边: %w", err)
	}
	return nil
}

// Graph 按方向与深度查询血缘子图。
//
// depth 被夹在 [1, MaxLineageDepth]：0 或负数按 1 处理（只要直接邻居），
// 超过上限按上限截断。上限是硬要求而不是保护性措施——血缘图可能带环
// （A 生成 B，B 又被拿去重跑出 A 的同款），无限深度的遍历在这种图上不会停。
//
// 遍历本身在这一层做去重，即使底层仓储把去重漏掉了，同一个节点也只会被
// 展开一次。
func (r *lineageRecorder) Graph(ctx context.Context, assetID string, dir domain.LineageDirection, depth int) (domain.LineageGraph, error) {
	if assetID == "" {
		return domain.LineageGraph{}, fmt.Errorf("asset: 血缘查询缺少 asset id")
	}
	if dir == "" {
		dir = domain.LineageBoth
	}
	switch dir {
	case domain.LineageAncestors, domain.LineageDescendants, domain.LineageBoth:
	default:
		return domain.LineageGraph{}, fmt.Errorf("asset: 未知的血缘方向 %q", dir)
	}
	depth = clampDepth(depth)

	g, err := r.assets.Lineage(ctx, assetID, dir, depth)
	if err != nil {
		return domain.LineageGraph{}, fmt.Errorf("asset: 查询资产 %s 的血缘: %w", assetID, err)
	}

	// 仓储可能按更宽松的规则返回（比如把整棵子树捞出来），这里按声明的
	// 深度与方向再收一次，保证接口的语义只由本层定义。
	return prune(g, assetID, dir, depth), nil
}

// clampDepth 把深度夹到 [1, MaxLineageDepth]。
func clampDepth(depth int) int {
	if depth < 1 {
		return 1
	}
	if depth > MaxLineageDepth {
		return MaxLineageDepth
	}
	return depth
}

// dedupeEdges 剔除自环与重复边，保持输入顺序。
func dedupeEdges(edges []domain.LineageEdge) []domain.LineageEdge {
	if len(edges) == 0 {
		return nil
	}
	type key struct {
		from, to string
		rel      domain.LineageRelation
	}
	seen := make(map[key]struct{}, len(edges))
	out := make([]domain.LineageEdge, 0, len(edges))
	for _, e := range edges {
		if e.From == "" || e.To == "" || e.From == e.To {
			continue
		}
		switch e.Relation {
		case domain.LineageInput, domain.LineageRerunOf, domain.LineageComposedFrom:
		default:
			continue
		}
		k := key{e.From, e.To, e.Relation}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}

// prune 从 root 出发按方向做一次有界 BFS，只保留可达的节点与边。
//
// 它是本层对「深度上限」这条契约的兑现点：仓储实现换成任何形态，
// 对外的图形状都由这里裁定。
func prune(g domain.LineageGraph, root string, dir domain.LineageDirection, depth int) domain.LineageGraph {
	up := dir == domain.LineageAncestors || dir == domain.LineageBoth
	down := dir == domain.LineageDescendants || dir == domain.LineageBoth

	// 邻接表：parents[to] 是它的全部父节点，children[from] 是它的全部子节点。
	parents := make(map[string][]domain.LineageEdge)
	children := make(map[string][]domain.LineageEdge)
	for _, e := range g.Edges {
		if e.From == "" || e.To == "" || e.From == e.To {
			continue
		}
		parents[e.To] = append(parents[e.To], e)
		children[e.From] = append(children[e.From], e)
	}

	visited := map[string]struct{}{root: {}}
	keptEdges := make([]domain.LineageEdge, 0, len(g.Edges))
	keptEdgeSet := make(map[domain.LineageEdge]struct{}, len(g.Edges))

	frontier := []string{root}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, node := range frontier {
			if up {
				for _, e := range parents[node] {
					if _, dup := keptEdgeSet[e]; !dup {
						keptEdgeSet[e] = struct{}{}
						keptEdges = append(keptEdges, e)
					}
					if _, seen := visited[e.From]; !seen {
						visited[e.From] = struct{}{}
						next = append(next, e.From)
					}
				}
			}
			if down {
				for _, e := range children[node] {
					if _, dup := keptEdgeSet[e]; !dup {
						keptEdgeSet[e] = struct{}{}
						keptEdges = append(keptEdges, e)
					}
					if _, seen := visited[e.To]; !seen {
						visited[e.To] = struct{}{}
						next = append(next, e.To)
					}
				}
			}
		}
		frontier = next
	}

	nodes := make([]domain.Asset, 0, len(visited))
	for _, n := range g.Nodes {
		if _, ok := visited[n.ID]; ok {
			nodes = append(nodes, n)
		}
	}
	return domain.LineageGraph{Nodes: nodes, Edges: keptEdges}
}

// InputEdges 为一次生成拼出 input 关系的边：每一件输入资产 → 每一件产物。
//
// 它放在本包而不是 executor 里，是因为「一次生成产生哪些边」是血缘的语义
// 而不是执行流程的细节；重跑、成片各有自己的构造函数，三者摆在一起
// 才能一眼看出三种关系的差别。
func InputEdges(inputAssetIDs []string, outputs []domain.Asset) []domain.LineageEdge {
	edges := make([]domain.LineageEdge, 0, len(inputAssetIDs)*len(outputs))
	for _, in := range inputAssetIDs {
		for _, out := range outputs {
			edges = append(edges, domain.LineageEdge{
				From:     in,
				To:       out.ID,
				Relation: domain.LineageInput,
			})
		}
	}
	return dedupeEdges(edges)
}

// RerunEdges 为单卡重跑拼出 rerun_of 边：新产物 ← 被替换的旧产物。
func RerunEdges(previousAssetID string, outputs []domain.Asset) []domain.LineageEdge {
	if previousAssetID == "" {
		return nil
	}
	edges := make([]domain.LineageEdge, 0, len(outputs))
	for _, out := range outputs {
		edges = append(edges, domain.LineageEdge{
			From:     previousAssetID,
			To:       out.ID,
			Relation: domain.LineageRerunOf,
		})
	}
	return dedupeEdges(edges)
}

// ComposedEdges 为一键成片拼出 composed_from 边：合成结果 ← 每一个片段。
func ComposedEdges(segmentAssetIDs []string, composed domain.Asset) []domain.LineageEdge {
	edges := make([]domain.LineageEdge, 0, len(segmentAssetIDs))
	for _, seg := range segmentAssetIDs {
		edges = append(edges, domain.LineageEdge{
			From:     seg,
			To:       composed.ID,
			Relation: domain.LineageComposedFrom,
		})
	}
	return dedupeEdges(edges)
}
