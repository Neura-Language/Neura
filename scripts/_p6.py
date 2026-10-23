p = 'internal/cluster/cluster_test.go'
s = open(p).read()
s = s.replace('''	resp, err := Call(node.Addr(), Request{Instance: "I", Method: "run"}, time.Second)''',
'''	resp, err := Call(node.Addr(), Request{Instance: "I", Method: "run", TraceID: "tr-42", Depth: 1}, time.Second)''')
s = s.replace('''	if string(resp.Trace) == "" {
		t.Fatal("响应应回传远端 trace 片段")
	}''', '''	if string(resp.Trace) == "" {
		t.Fatal("响应应回传远端 trace 片段")
	}
	if resp.TraceID != "tr-42" {
		t.Fatalf("trace_id 应原样回传，got %q", resp.TraceID)
	}
	if !strings.Contains(string(resp.Trace), `"trace_id":"tr-42"`) {
		t.Fatalf("远端片段应带 trace_id：%s", resp.Trace)
	}''')
open(p, 'w').write(s)

p = 'internal/runtime/net_test.go'
s = open(p).read()
s += '''
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
		t.Fatalf("trace records = %d, want 2（本地 dispatch + 远端 capability 各一条）\\n%+v", len(res.Trace), res.Trace)
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
	lines := strings.Split(strings.TrimSpace(buf.String()), "\\n")
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
'''

# imports for net_test
s = s.replace('''import (
	"strings"''', '''import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"''')

open(p, 'w').write(s)
print('tests appended')
