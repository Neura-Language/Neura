package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"neura/internal/cluster"
)

// 在两进程之外的最小验证：同一测试进程里起一个真实 TCP 节点，客户端走 RPC。
const netSrc = `
logic L {
  structure: { k: Int }
  capability: {
    run() -> Unit {
      learn(1)
    }
  }
}
instance I : L { structure: { k: 1 } }

fn go(x: L) -> Unit {
  match x against LogicDB {
    case L(e) => { invoke e.run() }
  }
}
`

func startClusterNode(t *testing.T, prog *Program) *cluster.Node {
	t.Helper()
	n := cluster.NewNode("127.0.0.1:0", prog, nil)
	if err := n.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go n.Serve()
	if prog != nil {
		prog.SetNodeAddr(n.Addr())
	}
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// 远端执行：客户端把 I.run() 发到节点，节点执行后返回 outputs/learns。
func TestRemoteCapabilityCall(t *testing.T) {
	serverProg := load(t, netSrc)
	node := startClusterNode(t, serverProg)

	clientProg := load(t, netSrc)
	clientProg.Table.Set("I", []string{node.Addr()})
	clientProg.EnableCluster("", 2*time.Second, nil)

	inst, ok := clientProg.DB.Lookup("I")
	if !ok {
		t.Fatal("instance I missing")
	}
	res := mustRun(t, clientProg, "go", []Value{InstV(inst)})

	if !contains(res.Outputs, "rpc I.run @"+node.Addr()) {
		t.Fatalf("outputs = %v, want rpc marker", res.Outputs)
	}
	if !contains(res.Learns, "learn 1") {
		t.Fatalf("learns = %v, want 远端 learn 回传", res.Learns)
	}
}

// failover：第一个节点打不通，自动转到第二个节点。
func TestRemoteFailover(t *testing.T) {
	serverProg := load(t, netSrc)
	node := startClusterNode(t, serverProg)

	dead := deadClusterAddr(t)

	clientProg := load(t, netSrc)
	clientProg.Table.Set("I", []string{dead, node.Addr()})

	var mu sync.Mutex
	var logs []string
	clientProg.EnableCluster("", 500*time.Millisecond, func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, f)
		mu.Unlock()
	})

	inst, _ := clientProg.DB.Lookup("I")
	res := mustRun(t, clientProg, "go", []Value{InstV(inst)})

	if !contains(res.Outputs, "rpc I.run @"+node.Addr()) {
		t.Fatalf("outputs = %v", res.Outputs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("failover 应产生日志")
	}
}

// 所有节点都失败 → 运行时错误（不静默）。
func TestRemoteAllNodesFail(t *testing.T) {
	clientProg := load(t, netSrc)
	clientProg.Table.Set("I", []string{deadClusterAddr(t)})
	clientProg.EnableCluster("", 200*time.Millisecond, nil)

	inst, _ := clientProg.DB.Lookup("I")
	_, err := clientProg.Run("go", []Value{InstV(inst)})
	if err == nil {
		t.Fatal("所有节点失败必须返回错误")
	}
	if !strings.Contains(err.Error(), "所有节点都失败") {
		t.Fatalf("err = %v", err)
	}
}

// 自己就是放置节点 → 本地执行，不起网络。
func TestSelfPlacementRunsLocal(t *testing.T) {
	prog := load(t, netSrc)
	// 先起一个节点拿到一个真实地址，再把它当作 self（不需要连它）
	node := startClusterNode(t, load(t, netSrc))
	prog.Table.Set("I", []string{node.Addr()})
	prog.EnableCluster(node.Addr(), time.Second, nil)

	inst, _ := prog.DB.Lookup("I")
	res := mustRun(t, prog, "go", []Value{InstV(inst)})
	if contains(res.Outputs, "rpc ") {
		t.Fatalf("命中 self 应本地执行，outputs = %v", res.Outputs)
	}
	if !contains(res.Outputs, "invoke I.run") {
		t.Fatalf("outputs = %v", res.Outputs)
	}
	if !contains(res.Learns, "learn 1") {
		t.Fatalf("learns = %v", res.Learns)
	}
}

// 节点内部执行时禁止再次转发（避免回路）：CallCapability 强制本地。
func TestServerSideExecutionNeverForwards(t *testing.T) {
	prog := load(t, netSrc)
	// 即使把自己登记为远端节点，CallCapability 也必须本地执行
	node := startClusterNode(t, prog)
	prog.Table.Set("I", []string{node.Addr()})
	prog.EnableCluster("", time.Second, nil)

	res, err := prog.CallCapability("I", "run", nil, "trace-test", 1)
	if err != nil {
		t.Fatalf("CallCapability: %v", err)
	}
	if len(res.Outputs) == 0 || !strings.HasPrefix(res.Outputs[0], "invoke I.run") {
		t.Fatalf("outs = %v", res.Outputs)
	}
	if len(res.Learns) != 1 {
		t.Fatalf("learns = %v", res.Learns)
	}
}

// 未知 instance / 不存在的 capability 在服务端要报错而不是崩。
func TestCallCapabilityErrors(t *testing.T) {
	prog := load(t, netSrc)
	if _, err := prog.CallCapability("Nope", "run", nil, "trace-test", 1); err == nil {
		t.Fatal("未知 instance 应报错")
	}
	// 未知方法只 warn 不 panic（与本地执行语义一致）
	prog2 := load(t, netSrc)
	if _, err := prog2.CallCapability("I", "nope", nil, "trace-test", 1); err != nil {
		t.Logf("unknown method returned error: %v", err)
	}
}

func deadClusterAddr(t *testing.T) string {
	t.Helper()
	n := cluster.NewNode("127.0.0.1:0", load(t, netSrc), nil)
	if err := n.Listen(); err != nil {
		t.Fatal(err)
	}
	addr := n.Addr()
	_ = n.Close()
	return addr
}

// ---------------- 全局 Trace 串联 ----------------

const traceSrc = `
logic Router {
  structure: { region: String }
  capability: {
    run() -> Unit {
      match self against LogicDB {
        case Router(r) => reuse r
      }
    }
  }
}
instance EU : Router { structure: { region: "eu" } }
instance US : Router { structure: { region: "us" } }

fn dispatch(x: Router) -> Unit {
  match x against LogicDB {
    case Router(e) => { invoke e.run() }
  }
}
`

// 发起端一条 trace，远端内部 match 产生的记录随响应回传并聚合，trace_id 相同、node 不同。
func TestTraceCorrelationAcrossNodes(t *testing.T) {
	serverProg := load(t, traceSrc)
	node := startClusterNode(t, serverProg)

	clientProg := load(t, traceSrc)
	clientProg.Table.Set("EU", []string{node.Addr()})
	clientProg.Table.Set("US", []string{node.Addr()})
	clientProg.EnableCluster("127.0.0.1:7002", 2*time.Second, nil)

	eu, ok := clientProg.DB.Lookup("EU")
	if !ok {
		t.Fatal("EU missing")
	}
	res := mustRun(t, clientProg, "dispatch", []Value{InstV(eu)})

	if len(res.Trace) != 2 {
		t.Fatalf("trace records = %d, want 2（本地 dispatch + 远端 capability 各一条）\n%+v", len(res.Trace), res.Trace)
	}
	if res.TraceID == "" {
		t.Fatal("Result.TraceID 不应为空")
	}
	for i, rec := range res.Trace {
		if rec.TraceID != res.TraceID {
			t.Fatalf("record[%d].TraceID = %q, want %q", i, rec.TraceID, res.TraceID)
		}
		if rec.Seq != i+1 {
			t.Fatalf("record[%d].Seq = %d, want %d（聚合后重排）", i, rec.Seq, i+1)
		}
	}
	if res.Trace[0].Node != "127.0.0.1:7002" {
		t.Fatalf("本地记录 node = %q", res.Trace[0].Node)
	}
	if res.Trace[1].Node != node.Addr() {
		t.Fatalf("远端记录 node = %q, want %q", res.Trace[1].Node, node.Addr())
	}
	if res.Trace[0].Depth != 0 || res.Trace[1].Depth != 1 {
		t.Fatalf("depth = %d/%d, want 0/1", res.Trace[0].Depth, res.Trace[1].Depth)
	}
	if res.Trace[1].Chosen != "EU" || res.Trace[1].Reason != "strict" {
		t.Fatalf("远端记录 = %+v", res.Trace[1])
	}
}

// trace 片段必须可 JSON 序列化并保持 trace_id（--trace 文件里能看到同一条链）。
func TestTraceJSONLCarriesTraceID(t *testing.T) {
	serverProg := load(t, traceSrc)
	node := startClusterNode(t, serverProg)

	clientProg := load(t, traceSrc)
	clientProg.Table.Set("EU", []string{node.Addr()})
	clientProg.Table.Set("US", []string{node.Addr()})
	clientProg.EnableCluster("127.0.0.1:7002", 2*time.Second, nil)

	eu, _ := clientProg.DB.Lookup("EU")
	res := mustRun(t, clientProg, "dispatch", []Value{InstV(eu)})

	var buf bytes.Buffer
	if err := clientProg.WriteTrace(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(res.Trace) {
		t.Fatalf("JSONL lines = %d, want %d", len(lines), len(res.Trace))
	}
	for i, line := range lines {
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec.TraceID != res.TraceID {
			t.Fatalf("line %d trace_id = %q, want %q", i, rec.TraceID, res.TraceID)
		}
	}
}

// ---------------- SIGHUP 热加载 ----------------

func hotSrc(version int) string {
	return fmt.Sprintf(`
logic Conf {
  structure: { version: Int }
  capability: {
    run() -> Unit {
      learn(self.version)
    }
  }
}
instance C : Conf { structure: { version: %d } }
fn go(x: Conf) -> Unit {
  match x against LogicDB {
    case Conf(e) => { invoke e.run() }
  }
}
`, version)
}

// Swap 后新请求立刻用新 Program，监听地址不变（进程不重启、连接不断）。
func TestHotReloadSwapsProgram(t *testing.T) {
	prog1 := load(t, hotSrc(1))
	sw := cluster.NewSwappable(prog1)
	node := cluster.NewNode("127.0.0.1:0", sw, nil)
	if err := node.Listen(); err != nil {
		t.Fatal(err)
	}
	go node.Serve()
	defer node.Close()
	addr := node.Addr()

	call := func() cluster.Response {
		resp, err := cluster.Call(addr, cluster.Request{Instance: "C", Method: "run"}, 2*time.Second)
		if err != nil {
			t.Fatalf("rpc: %v", err)
		}
		if !resp.OK {
			t.Fatalf("resp = %+v", resp)
		}
		return resp
	}

	r1 := call()
	if !contains(r1.Learns, "learn 1") {
		t.Fatalf("reload 前 learns = %v, want learn 1", r1.Learns)
	}

	prog2 := load(t, hotSrc(2))
	sw.Swap(prog2)

	if node.Addr() != addr {
		t.Fatalf("监听地址不应变化：%s → %s", addr, node.Addr())
	}
	r2 := call()
	if !contains(r2.Learns, "learn 2") {
		t.Fatalf("reload 后 learns = %v, want learn 2", r2.Learns)
	}

	// 旧 Program 不被热加载破坏
	if prog1.DB.Len() != 1 {
		t.Fatalf("old program 被改动: %d", prog1.DB.Len())
	}
}
