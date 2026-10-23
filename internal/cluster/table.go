package cluster

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoPlacement 表示该 instance 没有任何节点放置（应本地执行）。
var ErrNoPlacement = errors.New("cluster: instance 没有节点放置")

// ErrAllNodesFailed 表示 failover 链上的节点全部失败。
var ErrAllNodesFailed = errors.New("cluster: 所有节点都失败")

// Table 是静态节点表：instance → 节点地址列表（按声明顺序作为 failover 顺序）。
type Table struct {
	mu         sync.RWMutex
	byInstance map[string][]string
}

func NewTable() *Table {
	return &Table{byInstance: map[string][]string{}}
}

// Set 登记一个 instance 的节点列表（去重、保序）。
func (t *Table) Set(instance string, nodes []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	seen := map[string]bool{}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	t.byInstance[instance] = out
}

// Nodes 返回 node 列表的副本。
func (t *Table) Nodes(instance string) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]string(nil), t.byInstance[instance]...)
}

// Has 报告是否有放置。
func (t *Table) Has(instance string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byInstance[instance]) > 0
}

// Instances 返回所有已登记 instance（升序，便于日志）。
func (t *Table) Instances() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.byInstance))
	for k := range t.byInstance {
		out = append(out, k)
	}
	// 简单插入排序，避免额外 import 影响可读性
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Dispatcher 负责「按静态节点表直连 + 失败转移」。
type Dispatcher struct {
	Self    string        // 本节点地址；命中则本地执行
	Timeout time.Duration // 单节点超时
	Table   *Table
	Log     func(string, ...any)

	// RootKey 非空时，跨节点流量走 S2S 加密（握手 + AES-256-GCM）。
	// 为空保持原有明文 JSON-over-TCP，便于渐进启用。
	RootKey *RootKey
}

func (d *Dispatcher) logf(format string, a ...any) {
	if d.Log != nil {
		d.Log(format, a...)
	}
}

// Dispatch 按 failover 顺序调用：列表里若含 Self 则直接返回 Local（由 runtime 本地执行），
// 否则依次尝试各节点，失败（连接错、超时、对端 OK=false）就换下一个。
func (d *Dispatcher) Dispatch(instance, method string, args []Value, id, traceID string, depth int) (Response, string, error) {
	if d.Table == nil {
		return Response{}, "", ErrNoPlacement
	}
	nodes := d.Table.Nodes(instance)
	if len(nodes) == 0 {
		return Response{}, "", ErrNoPlacement
	}

	var lastErr error
	for i, node := range nodes {
		if d.Self != "" && node == d.Self {
			return Response{Local: true}, node, nil
		}
		req := Request{
			ID:       id,
			TraceID:  traceID,
			Depth:    depth,
			Instance: instance,
			Method:   method,
			Args:     args,
			From:     d.Self,
		}
		var (
			resp Response
			err  error
		)
		if d.RootKey != nil {
			resp, err = CallSecure(node, d.RootKey, req, d.Timeout)
		} else {
			resp, err = Call(node, req, d.Timeout)
		}
		if err == nil && resp.OK {
			if i > 0 {
				d.logf("failover: %s.%s 已转移到 %s（前 %d 个节点失败）", instance, method, node, i)
			}
			return resp, node, nil
		}
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", node, err)
		} else {
			lastErr = fmt.Errorf("%s: 远端错误: %s", node, resp.Error)
		}
		d.logf("failover: %s.%s @%s 失败（%v），尝试下一个", instance, method, node, lastErr)
	}
	return Response{}, "", fmt.Errorf("%w（%v）", ErrAllNodesFailed, lastErr)
}
