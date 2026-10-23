package runtime

import (
	"bytes"
	"testing"
	"time"

	"neura/internal/cluster"
)

// 跨层集成：runtime 的 capability RPC 走 S2S 加密（Node.EnableS2S +
// Program.EnableClusterS2S），验证 outputs/learns/trace 仍原样回传。
func TestRemoteCapabilityCallS2S(t *testing.T) {
	root, err := cluster.NewRootKey(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}

	serverProg := load(t, netSrc)
	node := cluster.NewNode("127.0.0.1:0", serverProg, nil)
	node.EnableS2S(root)
	if err := node.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go node.Serve()
	t.Cleanup(func() { _ = node.Close() })
	serverProg.SetNodeAddr(node.Addr())

	clientProg := load(t, netSrc)
	clientProg.Table.Set("I", []string{node.Addr()})
	clientProg.EnableClusterS2S("", 2*time.Second, root, nil)

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
	if len(res.Trace) == 0 {
		t.Fatal("加密链路也应回传 trace 片段")
	}
}
