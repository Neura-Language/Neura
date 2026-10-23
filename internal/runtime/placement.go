package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"neura/internal/ast"
	"neura/internal/place"
	"neura/internal/state"
)

// Placement 是 place 求解结果（观测/测试用）。
type Placement struct {
	Target  string
	Nodes   []string
	Adapter string
}

// SetPlacementAdapter 安装调度适配器。必须在 NewProgram 之前设置才会参与 place 求解；
// 运行期可通过 SetPlacementAdapter + Reschedule 热更新。
func (p *Program) SetPlacementAdapter(a place.Adapter) { p.placeAdapter = a }

// PlacementAdapter 返回当前 Adapter（可能为 nil）。
func (p *Program) PlacementAdapter() place.Adapter { return p.placeAdapter }

// Placement 查询单个 place 结果。
func (p *Program) Placement(name string) (Placement, bool) {
	pl, ok := p.placements[name]
	return pl, ok
}

// Scheduled 返回全部 place 求解快照。
func (p *Program) Scheduled() map[string]Placement {
	out := make(map[string]Placement, len(p.placements))
	for k, v := range p.placements {
		v.Nodes = append([]string(nil), v.Nodes...)
		out[k] = v
	}
	return out
}

// Reschedule 在 Adapter 节点快照变化后重跑所有 place 约束（滚动更新/节点下线）。
// 只更新节点表与 placed 元数据，不改已加载实例。
func (p *Program) Reschedule(ctx context.Context) error {
	if p.placeAdapter == nil {
		return fmt.Errorf("place: adapter not configured")
	}
	for _, d := range p.file.Decls {
		pd, ok := d.(*ast.PlaceDecl)
		if !ok || (pd.AtSpec != nil && pd.AtSpec.Local) {
			continue
		}
		p.applyAdapterCtx(ctx, pd)
	}
	return nil
}

// applyAdapter 是 NewProgram 阶段的同步求解（2s 上限，避免启动被外部节点源拖死）。
func (p *Program) applyAdapter(pd *ast.PlaceDecl) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.applyAdapterCtx(ctx, pd)
}

func (p *Program) applyAdapterCtx(ctx context.Context, pd *ast.PlaceDecl) bool {
	comp, err := place.Compile(pd.Where)
	if err != nil {
		p.warnf("place %s: where 编译失败（%v），回退静态节点表", pd.Name, err)
		return false
	}
	var at []string
	if pd.AtSpec != nil {
		at = append([]string(nil), pd.AtSpec.Nodes...)
	}
	req := place.Request{
		Target:  pd.Name,
		Where:   comp,
		Vars:    p.instanceVars(pd.Name),
		At:      at,
		Balance: balanceName(pd.Balance),
		HashKey: p.hashKey(pd),
		Self:    p.selfAddr,
	}
	chosen, err := p.placeAdapter.Match(ctx, req)
	if err != nil {
		p.warnf("place %s: adapter=%s 未找到节点（%v），回退静态节点表", pd.Name, p.placeAdapter.Name(), err)
		return false
	}
	p.Table.Set(pd.Name, chosen)
	p.placed[pd.Name] = "cluster"
	p.placements[pd.Name] = Placement{Target: pd.Name, Nodes: append([]string(nil), chosen...), Adapter: p.placeAdapter.Name()}
	p.warnf("place %s: adapter=%s where=%q nodes=%v", pd.Name, p.placeAdapter.Name(), comp, chosen)
	return true
}

// instanceVars 把 instance 字段暴露给 where（实例字段优先于节点指标）。
// 目标是 logic 时没有具体实例，字段约束应在 Adapter 层按节点指标表达。
func (p *Program) instanceVars(name string) map[string]any {
	in, ok := p.DB.Lookup(name)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(in.Fields))
	for k, v := range in.Fields {
		switch v.Kind {
		case KInt:
			out[k] = v.I
		case KFloat, KSize, KDuration:
			out[k] = v.F
		case KBool:
			out[k] = v.B
		case KString:
			out[k] = v.S
		case KInstance:
			if v.Inst != nil {
				out[k] = v.Inst.Name
			}
		default:
			out[k] = v.String()
		}
	}
	return out
}

// hashKey 取 consistent_hash(field) 的字段规范值；目标是 logic 时无法逐实例哈希，
// 返回空串让 Adapter 报错并回退静态表。
func (p *Program) hashKey(pd *ast.PlaceDecl) string {
	if pd.Balance == nil || pd.Balance.Kind != "consistent_hash" || len(pd.Balance.Args) == 0 {
		return ""
	}
	id, ok := pd.Balance.Args[0].(*ast.Ident)
	if !ok {
		return ""
	}
	in, ok := p.DB.Lookup(pd.Name)
	if !ok {
		return ""
	}
	v, ok := in.Field(id.Name)
	if !ok {
		return ""
	}
	return v.Canon()
}

func balanceName(p *ast.Policy) string {
	if p == nil {
		return ""
	}
	return p.Kind
}

// ---------------- 相似度 / 共享状态接线 ----------------

// SetSimilarityProvider 切换 LogicDB 相似度实现（Jaccard/SimHash/Embedding）。
func (p *Program) SetSimilarityProvider(s SimilarityProvider) { p.sim = s }

// SimilarityProvider 返回当前实现（nil 表示默认 Jaccard）。
func (p *Program) SimilarityProvider() SimilarityProvider { return p.sim }

func (p *Program) score(a, b map[string]Value) float64 {
	if p.sim == nil {
		return Similarity(a, b)
	}
	return p.sim.Score(a, b)
}

// providerTag 只对非默认实现打标，保持旧 trace 输出稳定。
func (p *Program) providerTag() string {
	if p.sim == nil {
		return ""
	}
	return p.sim.Name()
}

// EnableSharedState 接入跨节点乐观事务协调器；启用后 CrossNodeExactlyOnce 不再是 stub。
func (p *Program) EnableSharedState(c *state.Coordinator) { p.shared = c }

// SharedState 返回当前协调器。
func (p *Program) SharedState() *state.Coordinator { return p.shared }

// SortedPlacements 是稳定排序后的 place 列表（日志/CLI 用）。
func (p *Program) SortedPlacements() []Placement {
	out := make([]Placement, 0, len(p.placements))
	for _, v := range p.placements {
		v.Nodes = append([]string(nil), v.Nodes...)
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// FormatPlacement 只用于日志。
func FormatPlacement(p Placement) string {
	if len(p.Nodes) == 0 {
		return p.Target + " -> " + p.Adapter
	}
	return p.Target + " -> [" + strings.Join(p.Nodes, ", ") + "] (" + p.Adapter + ")"
}
