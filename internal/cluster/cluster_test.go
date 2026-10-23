package cluster

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------- 测试用执行器 ----------------

type fakeExec struct {
	mu    sync.Mutex
	calls int
	last  Request
	err   error
	delay time.Duration
}

func (f *fakeExec) CallCapability(instance, method string, args []Value, traceID string, depth int) (ExecResult, error) {
	f.mu.Lock()
	f.calls++
	f.last = Request{Instance: instance, Method: method, Args: args, TraceID: traceID, Depth: depth}
	delay := f.delay
	err := f.err
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return ExecResult{}, err
	}
	// 模拟远端产生一条 trace：验证 traceID 透传 + 片段回传
	trace := []byte(`[{"trace_id":"` + traceID + `","node":"fake","subject":"` + instance + `"}]`)
	return ExecResult{
		Outputs: []string{"invoke " + instance + "." + method},
		Learns:  []string{"learn 1"},
		Trace:   trace,
	}, nil
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func startNode(t *testing.T, exec Executor) *Node {
	t.Helper()
	n := NewNode("127.0.0.1:0", exec, t.Logf)
	if err := n.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go n.Serve()
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// deadAddr 返回一个「能拿到但立刻拒绝连接」的地址。
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// ---------------- 协议往返 ----------------

func TestRPCRoundTrip(t *testing.T) {
	exec := &fakeExec{}
	node := startNode(t, exec)

	resp, err := Call(node.Addr(), Request{Instance: "I", Method: "run", TraceID: "tr-42", Depth: 1}, time.Second)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Outputs) != 1 || resp.Outputs[0] != "invoke I.run" {
		t.Fatalf("outputs = %v", resp.Outputs)
	}
	if len(resp.Learns) != 1 || resp.Learns[0] != "learn 1" {
		t.Fatalf("learns = %v", resp.Learns)
	}
	if resp.Node != node.Addr() {
		t.Fatalf("node = %q, want %q", resp.Node, node.Addr())
	}
	if exec.count() != 1 {
		t.Fatalf("calls = %d, want 1", exec.count())
	}
	if string(resp.Trace) == "" {
		t.Fatal("响应应回传远端 trace 片段")
	}
	if resp.TraceID != "tr-42" {
		t.Fatalf("trace_id 应原样回传，got %q", resp.TraceID)
	}
	if !strings.Contains(string(resp.Trace), `"trace_id":"tr-42"`) {
		t.Fatalf("远端片段应带 trace_id：%s", resp.Trace)
	}
}

func TestBadRequestGetsErrorResponse(t *testing.T) {
	node := startNode(t, &fakeExec{})

	conn, err := net.DialTimeout("tcp", node.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("not-json\n")); err != nil {
		t.Fatal(err)
	}
	var resp Response
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "bad request") {
		t.Fatalf("resp = %+v", resp)
	}
}

// ---------------- 幂等表 ----------------

func TestIdempotencySequential(t *testing.T) {
	exec := &fakeExec{}
	node := startNode(t, exec)

	first, err := Call(node.Addr(), Request{ID: "k1", Instance: "I", Method: "run"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Call(node.Addr(), Request{ID: "k1", Instance: "I", Method: "run"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if exec.count() != 1 {
		t.Fatalf("同一 ID 只应执行一次，calls = %d", exec.count())
	}
	if first.Deduped {
		t.Fatal("第一次不应标 deduped")
	}
	if !second.Deduped {
		t.Fatal("第二次应标 deduped")
	}
	if len(second.Outputs) != 1 || second.Outputs[0] != first.Outputs[0] {
		t.Fatalf("重复请求应复用结果：%v vs %v", first.Outputs, second.Outputs)
	}
}

func TestIdempotencyConcurrent(t *testing.T) {
	exec := &fakeExec{delay: 50 * time.Millisecond}
	node := startNode(t, exec)

	const n = 8
	var wg sync.WaitGroup
	deduped := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := Call(node.Addr(), Request{ID: "same", Instance: "I", Method: "run"}, 2*time.Second)
			deduped[i] = resp.Deduped
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if exec.count() != 1 {
		t.Fatalf("并发同一 ID 只应执行一次，calls = %d", exec.count())
	}
	firsts := 0
	for _, d := range deduped {
		if !d {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("应恰好一个请求未标 deduped，got %d", firsts)
	}
}

func TestIdempotencyDifferentKeysAndEmpty(t *testing.T) {
	exec := &fakeExec{}
	node := startNode(t, exec)

	for _, id := range []string{"a", "b", ""} {
		if _, err := Call(node.Addr(), Request{ID: id, Instance: "I", Method: "run"}, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// a、b、"" 各自执行一次（空 ID 不去重）
	if exec.count() != 3 {
		t.Fatalf("calls = %d, want 3", exec.count())
	}
	// 再来一次空 ID → 仍执行
	_, _ = Call(node.Addr(), Request{Instance: "I", Method: "run"}, time.Second)
	if exec.count() != 4 {
		t.Fatalf("空 ID 不应去重，calls = %d", exec.count())
	}
}

func TestIdempotencyEviction(t *testing.T) {
	table := NewIdempotency(2)
	for _, k := range []string{"a", "b", "c"} {
		table.Do(k, func() Response { return Response{OK: true} })
	}
	if got := table.Len(); got > 2 {
		t.Fatalf("eviction 失效，Len = %d", got)
	}
}

// ---------------- failover ----------------

func TestFailoverToHealthyNode(t *testing.T) {
	exec := &fakeExec{}
	node := startNode(t, exec)
	dead := deadAddr(t)

	var logs []string
	var mu sync.Mutex
	d := &Dispatcher{
		Timeout: 500 * time.Millisecond,
		Table:   NewTable(),
		Log: func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, f)
			mu.Unlock()
		},
	}
	d.Table.Set("I", []string{dead, node.Addr()})

	resp, used, err := d.Dispatch("I", "run", nil, "id-1", "tr-1", 1)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !resp.OK || used != node.Addr() {
		t.Fatalf("resp=%+v used=%s", resp, used)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("failover 应有日志")
	}
}

func TestAllNodesFail(t *testing.T) {
	d := &Dispatcher{Timeout: 200 * time.Millisecond, Table: NewTable()}
	d.Table.Set("I", []string{deadAddr(t), deadAddr(t)})

	_, _, err := d.Dispatch("I", "run", nil, "id-2", "tr-2", 1)
	if !errors.Is(err, ErrAllNodesFailed) {
		t.Fatalf("err = %v, want ErrAllNodesFailed", err)
	}
}

func TestSelfPlacementIsLocal(t *testing.T) {
	exec := &fakeExec{}
	node := startNode(t, exec)
	d := &Dispatcher{Self: node.Addr(), Timeout: time.Second, Table: NewTable()}
	d.Table.Set("I", []string{node.Addr()})

	resp, used, err := d.Dispatch("I", "run", nil, "id-3", "tr-3", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Local || used != node.Addr() {
		t.Fatalf("resp=%+v used=%s", resp, used)
	}
	if exec.count() != 0 {
		t.Fatal("本地放置不应走网络")
	}
}

func TestNoPlacement(t *testing.T) {
	d := &Dispatcher{Timeout: time.Second, Table: NewTable()}
	_, _, err := d.Dispatch("missing", "run", nil, "id-4", "tr-4", 1)
	if !errors.Is(err, ErrNoPlacement) {
		t.Fatalf("err = %v, want ErrNoPlacement", err)
	}
}

// SIGHUP 热加载用的 Swappable：Swap 后立刻生效，旧执行器不再被使用。
func TestSwappableExecutor(t *testing.T) {
	a := &fakeExec{}
	b := &fakeExec{}
	sw := NewSwappable(a)

	if _, err := sw.CallCapability("I", "run", nil, "tr", 0); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 || b.count() != 0 {
		t.Fatalf("before swap: a=%d b=%d", a.count(), b.count())
	}

	sw.Swap(b)
	res, err := sw.CallCapability("I", "run", nil, "tr", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("after swap: a=%d b=%d", a.count(), b.count())
	}
	if len(res.Outputs) != 1 || res.Outputs[0] != "invoke I.run" {
		t.Fatalf("outputs = %v", res.Outputs)
	}
}

func TestTableOrderDedup(t *testing.T) {
	tbl := NewTable()
	tbl.Set("I", []string{"b", "a", "b", ""})
	got := tbl.Nodes("I")
	want := []string{"b", "a"}
	if len(got) != len(want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("nodes = %v, want %v", got, want)
		}
	}
	if !tbl.Has("I") || tbl.Has("X") {
		t.Fatal("Has 判定错误")
	}
}
