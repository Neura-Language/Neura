package place

import (
	"context"
	"fmt"
	"sync/atomic"
)

// EventKind 是节点快照的增量类型（对齐 K8s watch 语义）。
type EventKind int

const (
	EventAdd EventKind = iota
	EventUpdate
	EventDelete
)

// Event 是 NodeSource 的增量事件。
type Event struct {
	Kind EventKind
	Node NodeInfo
}

// NodeSource 是「节点清单从哪来」的最小抽象。
//
// 未来接 K8s 时，用 client-go 的 SharedInformer 实现 List/Watch 即可：
//   - List:  lister.List(labels.Everything()) → 转 NodeInfo
//   - Watch: informer.AddEventHandler → 投递 Event
//
// place 包本身不 import client-go，主二进制不背 K8s SDK。
type NodeSource interface {
	List(ctx context.Context) ([]NodeInfo, error)
	Watch(ctx context.Context) (<-chan Event, error)
}

// RemoteAdapter 把外部 NodeSource 适配成 Adapter。
// Match 直接复用 LabelAdapter 的约束求解，只把节点来源换成 Source。
type RemoteAdapter struct {
	Source NodeSource
	Base   *LabelAdapter
	Log    func(string, ...any)

	loaded atomic.Bool // List/Watch 已建立初始快照；此后 Match 不再全量重拉
}

// NewRemoteAdapter 创建外部节点源适配器。
func NewRemoteAdapter(src NodeSource) *RemoteAdapter {
	return &RemoteAdapter{Source: src, Base: NewLabelAdapter()}
}

func (r *RemoteAdapter) Name() string { return "remote" }

// Nodes 拉取一次全量并刷新 Base。
func (r *RemoteAdapter) Nodes(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := r.Source.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("place: list nodes: %w", err)
	}
	r.Base.SetNodes(nodes)
	r.loaded.Store(true)
	return r.Base.Nodes(ctx)
}

// Match 实现 Adapter。首次调用做全量；之后若 Run 在消费 watch，
// 直接读 Base 的增量快照，避免每次调度都打 K8s apiserver。
func (r *RemoteAdapter) Match(ctx context.Context, req Request) ([]string, error) {
	if !r.loaded.Load() {
		if _, err := r.Nodes(ctx); err != nil {
			return nil, err
		}
	}
	return r.Base.Match(ctx, req)
}

// Run 消费 Source 的 watch 事件并增量维护 Base，直到 ctx 取消。
// 可选；不调用时 Match 首次全量 List 后使用静态快照。
func (r *RemoteAdapter) Run(ctx context.Context) error {
	if !r.loaded.Load() {
		if _, err := r.Nodes(ctx); err != nil {
			return err
		}
	}
	ch, err := r.Source.Watch(ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			switch ev.Kind {
			case EventDelete:
				r.Base.Delete(ev.Node.Name)
			default:
				r.Base.Upsert(ev.Node)
			}
			if r.Log != nil {
				r.Log("place: watch %v %s", ev.Kind, ev.Node.Name)
			}
		}
	}
}
