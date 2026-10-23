package state

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"time"
)

// ErrIndeterminate 表示部分节点 commit 未被确认。调用方应使用同一 TxID 重试：
// 参与者按 txID 幂等，重试只会补齐未确认节点，不会重复应用。
var ErrIndeterminate = errors.New("state: commit indeterminate")

// ErrTxIDReuse 表示同一 TxID 被用于不同的写集合。幂等键必须唯一标识一次事务。
var ErrTxIDReuse = errors.New("state: txID reused with different writes")

// Txn 是一次乐观事务。ID 必须全局唯一且跨重试稳定：
// 推荐 "traceID#seq"（例如 "n1-t7#3"），它是 exactly-once 的幂等键。
type Txn struct {
	ID      string
	TraceID string
	Writes  []Write
}

// NewTxn 用全局 TraceID + 单调序号构造幂等事务。
func NewTxn(traceID string, seq uint64) *Txn {
	return &Txn{
		ID:      traceID + "#" + strconv.FormatUint(seq, 10),
		TraceID: traceID,
	}
}

// Router 按键决定主副本节点（单写者不变式）。返回 "" 表示本地。
type Router func(key string) string

// Peer 是跨节点状态 RPC 的最小传输抽象。
// 生产实现必须走 S2S 加密（见 S2SPeer）；内存实现只允许出现在测试。
type Peer interface {
	Do(ctx context.Context, node string, rpc RPC) (Reply, error)
}

// Result 是一次提交的汇总。Indeterminate 非空时返回 ErrIndeterminate。
type Result struct {
	TxID          string
	TraceID       string
	Records       map[string]Record
	Indeterminate []string
	Deduped       bool
	Attempts      int

	fingerprint string // 写集合指纹，防止 TxID 复用
}

func (r *Result) Versions() map[string]Version {
	out := make(map[string]Version, len(r.Records))
	for k, rec := range r.Records {
		out[k] = rec.Version
	}
	return out
}

// Coordinator 是轻量级跨节点协调器：
//
//	Reserve（全部节点）→ Commit（全部节点）
//
// 没有 leader/日志复制：每键单一 owner，租约互斥冲突，失败靠 TTL 回收 + 同 TxID 幂等重试收敛。
// 提供的是「每键 exactly-once 状态效果 + 最终一致」，不是线性一致多键原子性。
type Coordinator struct {
	Self     string
	Store    *Store
	Replica  *Replica
	Peer     Peer
	Route    Router
	TTL      time.Duration       // 预留租约
	Attempts int                 // CommitFn 遇到冲突时的最大尝试次数
	Sleep    func(time.Duration) // 测试可注入；nil 用 time.Sleep

	applied *Window
}

// NewCoordinator 创建协调器。local 必须是本节点 Replica 使用的同一个 Store。
func NewCoordinator(self string, local *Store, peer Peer, route Router) *Coordinator {
	if route == nil {
		route = func(string) string { return self }
	}
	return &Coordinator{
		Self:     self,
		Store:    local,
		Replica:  NewReplica(local),
		Peer:     peer,
		Route:    route,
		TTL:      15 * time.Second,
		Attempts: 3,
		applied:  NewWindow(8192, 10*time.Minute),
	}
}

// Commit 执行一次事务。同一 TxID 的并发/重复提交只会真正应用一次。
func (c *Coordinator) Commit(ctx context.Context, tx *Txn) (*Result, error) {
	if tx == nil || tx.ID == "" || len(tx.Writes) == 0 {
		return nil, ErrNoTxn
	}
	fp := writeFingerprint(tx.Writes)
	v, hit, err := c.applied.Do(tx.ID, func() (any, error) {
		return c.commitOnce(ctx, tx)
	})
	if err != nil {
		return nil, err
	}
	res := v.(*Result)
	if res.fingerprint != fp {
		return nil, fmt.Errorf("%w: %s", ErrTxIDReuse, tx.ID)
	}
	out := *res // 缓存值只读；每个调用方拿到独立副本，避免 Deduped 竞争。
	out.Deduped = hit
	return &out, nil
}

func (c *Coordinator) commitOnce(ctx context.Context, tx *Txn) (*Result, error) {
	groups := map[string][]Write{}
	keys := map[string][]string{}
	for _, w := range tx.Writes {
		if w.Key == "" {
			return nil, fmt.Errorf("state: empty key in tx %s", tx.ID)
		}
		node := c.nodeOf(w.Key)
		groups[node] = append(groups[node], w)
		keys[node] = append(keys[node], w.Key)
	}
	nodes := make([]string, 0, len(groups))
	for n := range groups {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	// Phase 1: reserve（每个节点内原子，跨节点失败则全部 abort）。
	reserved := make([]string, 0, len(nodes))
	for _, node := range nodes {
		rep, err := c.call(ctx, node, RPC{
			Kind: RPCReserve, TxID: tx.ID, TraceID: tx.TraceID,
			Writes: groups[node], TTL: c.ttl(),
		})
		if err != nil {
			c.abortAll(ctx, nodes, tx.ID, keys)
			return nil, normalize(err)
		}
		_ = rep
		reserved = append(reserved, node)
	}

	// Phase 2: commit（同 TxID 幂等；部分失败返回 ErrIndeterminate 供重试收敛）。
	res := &Result{TxID: tx.ID, TraceID: tx.TraceID, Records: map[string]Record{},
		fingerprint: writeFingerprint(tx.Writes)}
	var lastErr error
	for _, node := range nodes {
		rep, err := c.call(ctx, node, RPC{
			Kind: RPCCommit, TxID: tx.ID, TraceID: tx.TraceID, Keys: keys[node],
		})
		if err != nil {
			res.Indeterminate = append(res.Indeterminate, node)
			lastErr = err
			continue
		}
		for _, rec := range rep.Records {
			res.Records[rec.Key] = rec
		}
	}
	if len(res.Indeterminate) > 0 {
		// 未确认节点上的租约到期后可被其他事务回收；重新提交同 TxID 会命中 applied 窗口并补齐。
		return res, fmt.Errorf("%w: nodes=%v: %v", ErrIndeterminate, res.Indeterminate, lastErr)
	}
	return res, nil
}

// CommitFn 在版本冲突时重建事务并重试。build 每次都必须读最新版本。
// TxID 由调用方决定；若为空，使用 traceID#attempt 保证跨重试唯一且可观测。
func (c *Coordinator) CommitFn(ctx context.Context, traceID string, build func(ctx context.Context, attempt int) (*Txn, error)) (*Result, error) {
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		tx, err := build(ctx, i)
		if err != nil {
			return nil, err
		}
		if tx.ID == "" {
			tx.ID = fmt.Sprintf("%s#%d", traceID, i+1)
		}
		if tx.TraceID == "" {
			tx.TraceID = traceID
		}
		res, err := c.Commit(ctx, tx)
		if err == nil {
			res.Attempts = i + 1
			return res, nil
		}
		if !errors.Is(err, ErrConflict) {
			return res, err
		}
		lastErr = err
		c.sleep(time.Duration(1<<uint(i)) * time.Millisecond)
	}
	return nil, fmt.Errorf("state: %d attempts exhausted: %w", attempts, lastErr)
}

// Reconcile 从键的 owner 拉取最新记录并 Merge 到本地（反熵/读修复）。
func (c *Coordinator) Reconcile(ctx context.Context, key string) (Record, bool, error) {
	node := c.nodeOf(key)
	rep, err := c.call(ctx, node, RPC{Kind: RPCGet, Keys: []string{key}})
	if err != nil {
		return Record{}, false, err
	}
	if len(rep.Records) == 0 {
		return Record{}, false, nil
	}
	c.Store.Merge(rep.Records)
	return rep.Records[0], true, nil
}

func (c *Coordinator) nodeOf(key string) string {
	n := c.Route(key)
	if n == "" {
		return c.Self
	}
	return n
}

func (c *Coordinator) call(ctx context.Context, node string, rpc RPC) (Reply, error) {
	if node == "" || node == c.Self {
		return c.Replica.HandleRPC(rpc)
	}
	if c.Peer == nil {
		return Reply{}, fmt.Errorf("state: no peer for remote node %s", node)
	}
	rep, err := c.Peer.Do(ctx, node, rpc)
	if err != nil {
		return Reply{}, err
	}
	if err := rep.Err(); err != nil {
		return Reply{}, err
	}
	return rep, nil
}

func (c *Coordinator) abortAll(ctx context.Context, nodes []string, txID string, keys map[string][]string) {
	for _, node := range nodes {
		_, _ = c.call(ctx, node, RPC{Kind: RPCAbort, TxID: txID, Keys: keys[node]})
	}
}

func (c *Coordinator) ttl() time.Duration {
	if c.TTL <= 0 {
		return 15 * time.Second
	}
	return c.TTL
}

func (c *Coordinator) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

func normalize(err error) error {
	var ce *ConflictError
	if errors.As(err, &ce) {
		return ce
	}
	return err
}

// writeFingerprint 是写集合的确定性指纹（与 Writes 顺序无关）。
func writeFingerprint(writes []Write) string {
	ws := append([]Write(nil), writes...)
	sort.Slice(ws, func(i, j int) bool { return ws[i].Key < ws[j].Key })
	h := fnv.New64a()
	for _, w := range ws {
		fmt.Fprintf(h, "%s\x00%d\x00%t\x00", w.Key, w.Expect, w.Delete)
		h.Write(w.Value)
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// LocalRouter 把一组键固定映射到本节点（单机回退）。
func LocalRouter(self string) Router {
	return func(string) string { return self }
}

// MapRouter 是测试/静态分片用：key→node 精确匹配，未命中回退 self。
func MapRouter(self string, m map[string]string) Router {
	return func(key string) string {
		if n, ok := m[key]; ok {
			return n
		}
		return self
	}
}
