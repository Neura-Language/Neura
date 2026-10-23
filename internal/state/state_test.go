package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"neura/internal/cluster"
)

// ---------------- 内存网络 ----------------

type memNet struct {
	mu    sync.RWMutex
	nodes map[string]*Replica
}

func newMemNet() *memNet { return &memNet{nodes: map[string]*Replica{}} }

func (m *memNet) add(name string, s *Store) {
	m.mu.Lock()
	m.nodes[name] = NewReplica(s)
	m.mu.Unlock()
}

func (m *memNet) Do(_ context.Context, node string, rpc RPC) (Reply, error) {
	m.mu.RLock()
	r := m.nodes[node]
	m.mu.RUnlock()
	if r == nil {
		return Reply{}, fmt.Errorf("no node %s", node)
	}
	return r.HandleRPC(rpc)
}

func mustWrite(t *testing.T, s *Store, key string, expect Version, val string) {
	t.Helper()
	txID := fmt.Sprintf("seed-%s-%d", key, expect)
	if _, err := s.Reserve(txID, []Write{{Key: key, Expect: expect, Value: []byte(val)}}, time.Minute); err != nil {
		t.Fatalf("seed reserve %s: %v", key, err)
	}
	if _, _, err := s.Commit(txID, "seed", []string{key}); err != nil {
		t.Fatalf("seed commit %s: %v", key, err)
	}
}

// ---------------- Store ----------------

func TestStoreCASCommitIsIdempotent(t *testing.T) {
	s := NewStore(64, time.Minute)
	tx := "t#1"
	if _, err := s.Reserve(tx, []Write{{Key: "k", Expect: 0, Value: []byte("v1")}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	res, applied, err := s.Commit(tx, "trace-1", []string{"k"})
	if err != nil || applied {
		t.Fatalf("commit: %v applied=%v", err, applied)
	}
	if got := res.Records["k"].Version; got != 1 {
		t.Fatalf("version = %d, want 1", got)
	}
	// 重复提交（例如 RPC 超时后重试）必须返回缓存，且不再 +1。
	again, applied, err := s.Commit(tx, "trace-1", []string{"k"})
	if err != nil || !applied {
		t.Fatalf("dedup commit: %v applied=%v", err, applied)
	}
	if again.Records["k"].Version != 1 {
		t.Fatalf("version after replay = %d", again.Records["k"].Version)
	}
	rec, _ := s.Get("k")
	if rec.Version != 1 || string(rec.Value) != "v1" {
		t.Fatalf("record = %+v", rec)
	}

	// 新一代仍按 CAS。
	if _, err := s.Reserve("t#2", []Write{{Key: "k", Expect: 0, Value: []byte("bad")}}, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if _, err := s.Reserve("t#2", []Write{{Key: "k", Expect: 1, Value: []byte("v2")}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Commit("t#2", "trace-2", []string{"k"}); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.Get("k")
	if rec.Version != 2 || string(rec.Value) != "v2" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestStoreAbortReleasesReservation(t *testing.T) {
	s := NewStore(16, time.Minute)
	if _, err := s.Reserve("a", []Write{{Key: "k", Expect: 0, Value: []byte("x")}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// 其他事务在租约期内必须冲突。
	if _, err := s.Reserve("b", []Write{{Key: "k", Expect: 0, Value: []byte("y")}}, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected reserved conflict, got %v", err)
	}
	s.Abort("a", []string{"k"})
	if _, err := s.Reserve("b", []Write{{Key: "k", Expect: 0, Value: []byte("y")}}, time.Minute); err != nil {
		t.Fatalf("after abort: %v", err)
	}
}

// ---------------- Window ----------------

func TestWindowSingleflightAndNoErrorCache(t *testing.T) {
	w := NewWindow(8, time.Minute)
	var calls atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, err := w.Do("same", func() (any, error) {
				calls.Add(1)
				time.Sleep(5 * time.Millisecond)
				return 42, nil
			})
			if err != nil || v.(int) != 42 {
				t.Errorf("do = %v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	// 失败不缓存。
	if _, _, err := w.Do("bad", func() (any, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := w.Do("bad", func() (any, error) { return 1, nil }); err != nil {
		t.Fatalf("retry after error: %v", err)
	}
}

// ---------------- Coordinator ----------------

func TestCoordinatorConflictAbortsAllNodes(t *testing.T) {
	sa, sb := NewStore(32, time.Minute), NewStore(32, time.Minute)
	mustWrite(t, sb, "k2", 0, "existing")

	net := newMemNet()
	net.add("a", sa)
	net.add("b", sb)
	c := NewCoordinator("a", sa, net, MapRouter("a", map[string]string{"k1": "a", "k2": "b"}))
	tx := &Txn{ID: "trace#1", TraceID: "trace", Writes: []Write{
		{Key: "k1", Expect: 0, Value: []byte("v1")},
		{Key: "k2", Expect: 0, Value: []byte("v2")}, // 真实版本是 1 → 冲突
	}}
	if _, err := c.Commit(context.Background(), tx); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want conflict", err)
	}
	if _, ok := sa.Get("k1"); ok {
		t.Fatal("k1 must be rolled back (no partial commit)")
	}
	if _, err := sa.Reserve("other", []Write{{Key: "k1", Expect: 0, Value: []byte("x")}}, time.Minute); err != nil {
		t.Fatalf("k1 still reserved after abort: %v", err)
	}
}

func TestCoordinatorExactlyOnceUnderConcurrency(t *testing.T) {
	s := NewStore(32, time.Minute)
	c := NewCoordinator("a", s, nil, LocalRouter("a"))
	tx := &Txn{ID: "trace#7", TraceID: "trace", Writes: []Write{
		{Key: "k", Expect: 0, Value: []byte("v")},
	}}

	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Commit(context.Background(), tx); err == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 32 {
		t.Fatalf("success = %d, want 32", ok.Load())
	}
	rec, exists := s.Get("k")
	if !exists || rec.Version != 1 {
		t.Fatalf("record = %+v, want version 1", rec)
	}
}

// flakyPeer 让指定节点的 commit 失败 N 次，用于验证同 TxID 重试收敛。
type flakyPeer struct {
	inner Peer
	mu    sync.Mutex
	fail  map[string]int
}

func (f *flakyPeer) Do(ctx context.Context, node string, rpc RPC) (Reply, error) {
	f.mu.Lock()
	if rpc.Kind == RPCCommit && f.fail[node] > 0 {
		f.fail[node]--
		f.mu.Unlock()
		return Reply{}, errors.New("simulated link reset")
	}
	f.mu.Unlock()
	return f.inner.Do(ctx, node, rpc)
}

func TestIndeterminateRetryConvergesWithoutDoubleApply(t *testing.T) {
	sa, sb := NewStore(32, time.Minute), NewStore(32, time.Minute)
	mem := newMemNet()
	mem.add("a", sa)
	mem.add("b", sb)
	peer := &flakyPeer{inner: mem, fail: map[string]int{"b": 1}}
	c := NewCoordinator("a", sa, peer, MapRouter("a", map[string]string{"k1": "a", "k2": "b"}))
	tx := &Txn{ID: "trace#9", TraceID: "trace", Writes: []Write{
		{Key: "k1", Expect: 0, Value: []byte("v1")},
		{Key: "k2", Expect: 0, Value: []byte("v2")},
	}}
	if _, err := c.Commit(context.Background(), tx); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("first commit err = %v, want indeterminate", err)
	}
	res, err := c.Commit(context.Background(), tx)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(res.Indeterminate) != 0 {
		t.Fatalf("indeterminate = %v", res.Indeterminate)
	}
	for node, s := range map[string]*Store{"a": sa, "b": sb} {
		rec, ok := s.Get("k" + map[string]string{"a": "1", "b": "2"}[node])
		if !ok || rec.Version != 1 {
			t.Fatalf("node %s record = %+v", node, rec)
		}
	}
}

func TestTxIDReuseRejected(t *testing.T) {
	s := NewStore(16, time.Minute)
	c := NewCoordinator("a", s, nil, LocalRouter("a"))
	tx := &Txn{ID: "trace#1", TraceID: "trace", Writes: []Write{{Key: "k", Expect: 0, Value: []byte("v1")}}}
	if _, err := c.Commit(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	bad := &Txn{ID: "trace#1", TraceID: "trace", Writes: []Write{{Key: "k", Expect: 1, Value: []byte("v2")}}}
	if _, err := c.Commit(context.Background(), bad); !errors.Is(err, ErrTxIDReuse) {
		t.Fatalf("err = %v, want ErrTxIDReuse", err)
	}
	rec, _ := s.Get("k")
	if string(rec.Value) != "v1" || rec.Version != 1 {
		t.Fatalf("record mutated: %+v", rec)
	}
}

// ---------------- S2S 端到端 ----------------

func TestStateRPCOverS2SOnly(t *testing.T) {
	root, err := cluster.GenerateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	storeA, storeB := NewStore(32, time.Minute), NewStore(32, time.Minute)
	nodeA := startStateNode(t, storeA, root)
	nodeB := startStateNode(t, storeB, root)

	c := NewCoordinator(nodeA, storeA, NewS2SPeer(nodeA, root, 2*time.Second),
		MapRouter(nodeA, map[string]string{"remote": nodeB}))
	tx := &Txn{ID: "trace-s2s#1", TraceID: "trace-s2s", Writes: []Write{
		{Key: "remote", Expect: 0, Value: []byte("secret")},
	}}
	res, err := c.Commit(context.Background(), tx)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Records["remote"].Version != 1 {
		t.Fatalf("result = %+v", res)
	}
	rec, ok := storeB.Get("remote")
	if !ok || string(rec.Value) != "secret" {
		t.Fatalf("remote record = %+v", rec)
	}

	// 同一 TxID 重试不能重复应用。
	if _, err := c.Commit(context.Background(), tx); err != nil {
		t.Fatalf("replay: %v", err)
	}
	rec, _ = storeB.Get("remote")
	if rec.Version != 1 {
		t.Fatalf("version after replay = %d", rec.Version)
	}

	// 无 RootKey 的 Peer 必须直接拒绝，禁止明文降级。
	if _, err := NewS2SPeer(nodeA, nil, time.Second).Do(context.Background(), nodeB, RPC{Kind: RPCGet, Keys: []string{"remote"}}); !errors.Is(err, ErrPlaintext) {
		t.Fatalf("plaintext peer err = %v", err)
	}

	// 直接明文 TCP 打控制面端口必须失败（节点只接受 S2S 握手）。
	if _, err := cluster.Call(nodeB, cluster.Request{Op: "state.get", Payload: []byte("{}")}, 300*time.Millisecond); err == nil {
		t.Fatal("plaintext request to S2S node must fail")
	}
}

func startStateNode(t *testing.T, s *Store, root *cluster.RootKey) string {
	t.Helper()
	n := cluster.NewNode("127.0.0.1:0", nil, nil)
	n.EnableS2S(root)
	n.SetOpsHandler(NewReplica(s).Handle)
	if err := n.Listen(); err != nil {
		t.Fatal(err)
	}
	go n.Serve()
	t.Cleanup(func() { _ = n.Close() })
	return n.Addr()
}
