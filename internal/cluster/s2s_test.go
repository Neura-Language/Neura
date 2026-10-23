package cluster

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------- 测试辅助 ----------------

func mustRootKey(t *testing.T, seed byte) *RootKey {
	t.Helper()
	rk, err := NewRootKey(bytes.Repeat([]byte{seed}, rootKeyLen))
	if err != nil {
		t.Fatalf("NewRootKey: %v", err)
	}
	return rk
}

func startSecureNode(t *testing.T, exec Executor, root *RootKey) *Node {
	t.Helper()
	n := NewNode("127.0.0.1:0", exec, t.Logf)
	n.EnableS2S(root)
	if err := n.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go n.Serve()
	t.Cleanup(func() { _ = n.Close() })
	return n
}

// dialSecureRaw 完成一次真实握手，返回连接与双方一致的会话。
func dialSecureRaw(t *testing.T, addr string, root *RootKey) (net.Conn, *bufio.Reader, *Session) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	br := bufio.NewReader(conn)
	hs := BeginClientHandshake(conn, br, root, 2*time.Second, t.Logf)
	sess, err := hs.WaitTimeout(2 * time.Second)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("handshake: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, br, sess
}

func containsState(states []HandshakeState, want HandshakeState) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------- Root Key / HKDF ----------------

func TestParseRootKeyFormats(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, rootKeyLen)
	hexStr := "abababababababababababababababababababababababababababababababab"
	b64Str := base64.StdEncoding.EncodeToString(raw)

	for _, in := range []string{hexStr, "hex:" + hexStr, b64Str, "base64:" + b64Str, "  " + hexStr + "\n"} {
		rk, err := ParseRootKey(in)
		if err != nil {
			t.Fatalf("ParseRootKey(%q): %v", in, err)
		}
		if !bytes.Equal(rk.k[:], raw) {
			t.Fatalf("ParseRootKey(%q) 解出的密钥不匹配", in)
		}
		if rk.String() == hexStr || strings.Contains(rk.String(), hexStr) {
			t.Fatalf("String() 不应泄露密钥：%q", rk.String())
		}
		if len(rk.Fingerprint()) != 16 {
			t.Fatalf("fingerprint = %q", rk.Fingerprint())
		}
	}

	for _, in := range []string{"", "abc", "hex:zz", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParseRootKey(in); err == nil {
			t.Fatalf("ParseRootKey(%q) 应失败", in)
		}
	}
}

// HKDF 派生必须确定、双向对称；换 NewSalt 后旧密钥必须解不开新会话。
func TestDeriveSessionDirectional(t *testing.T) {
	root := mustRootKey(t, 0x11)
	salt := bytes.Repeat([]byte{0x22}, newSaltLen)

	client, err := deriveSession(root, salt, true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := deriveSession(root, salt, true)
	if err != nil {
		t.Fatal(err)
	}
	server, err := deriveSession(root, salt, false)
	if err != nil {
		t.Fatal(err)
	}
	if client.Fingerprint() != again.Fingerprint() || client.Fingerprint() != server.Fingerprint() {
		t.Fatal("同一 RootKey+NewSalt 的会话指纹应一致")
	}

	aad := []byte("test-aad")
	nonce, sealed, err := sealAEAD(client.send, []byte("payload"), aad)
	if err != nil {
		t.Fatal(err)
	}
	// A 的发送密钥 == B 的接收密钥
	pt, err := openAEAD(server.recv, nonce, sealed, aad)
	if err != nil || string(pt) != "payload" {
		t.Fatalf("c2s 方向派生不对称: pt=%q err=%v", pt, err)
	}
	// A 的接收密钥 == B 的发送密钥
	nonce2, sealed2, err := sealAEAD(server.send, []byte("reply"), aad)
	if err != nil {
		t.Fatal(err)
	}
	pt2, err := openAEAD(client.recv, nonce2, sealed2, aad)
	if err != nil || string(pt2) != "reply" {
		t.Fatalf("s2c 方向派生不对称: pt=%q err=%v", pt2, err)
	}
	// A 的接收密钥不能解 A 自己发出的密文（方向隔断）
	if _, err := openAEAD(client.recv, nonce, sealed, aad); err == nil {
		t.Fatal("方向密钥不应可互换")
	}

	otherSalt := bytes.Repeat([]byte{0x33}, newSaltLen)
	other, err := deriveSession(root, otherSalt, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openAEAD(other.recv, nonce2, sealed2, aad); err == nil {
		t.Fatal("不同 NewSalt 派生出的会话不应能解密")
	}
}

// ---------------- 防重放 TTL Map ----------------

func TestReplayCacheWindowAndTTL(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	rp := NewReplayCache(time.Second)
	rp.now = func() time.Time { return base }

	ts := base.Unix()
	if err := rp.CheckAndAdd(ts, "d:n1"); err != nil {
		t.Fatalf("首次 nonce 应接受: %v", err)
	}
	if err := rp.CheckAndAdd(ts, "d:n1"); !errors.Is(err, ErrS2SReplay) {
		t.Fatalf("重复 nonce err = %v, want ErrS2SReplay", err)
	}
	if err := rp.CheckAndAdd(ts-4, "d:n2"); !errors.Is(err, ErrS2SStale) {
		t.Fatalf("过期时间戳 err = %v, want ErrS2SStale", err)
	}
	if err := rp.CheckAndAdd(ts+4, "d:n3"); !errors.Is(err, ErrS2SStale) {
		t.Fatalf("未来时间戳 err = %v, want ErrS2SStale", err)
	}
	if err := rp.CheckAndAdd(ts, ""); !errors.Is(err, ErrS2SFrame) {
		t.Fatalf("空 nonce err = %v, want ErrS2SFrame", err)
	}

	// TTL=1s：1.5s 后条目过期，但时间戳仍在 3s 窗口内 → 可再次接受。
	base = base.Add(1500 * time.Millisecond)
	if err := rp.CheckAndAdd(base.Unix(), "d:n1"); err != nil {
		t.Fatalf("TTL 过期后应可重新接受: %v", err)
	}
	if rp.TTL() != time.Second {
		t.Fatalf("TTL = %s", rp.TTL())
	}
}

// ---------------- 端到端加密往返 ----------------

func TestSecureRoundTrip(t *testing.T) {
	root := mustRootKey(t, 0x42)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)

	resp, err := CallSecure(node.Addr(), root, Request{
		ID: "s2s-1", Instance: "I", Method: "run", TraceID: "tr-s2s", Depth: 2,
	}, 2*time.Second)
	if err != nil {
		t.Fatalf("CallSecure: %v", err)
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
	if resp.TraceID != "tr-s2s" || !strings.Contains(string(resp.Trace), `"trace_id":"tr-s2s"`) {
		t.Fatalf("trace 未透传: id=%q trace=%s", resp.TraceID, resp.Trace)
	}
	if resp.Node != node.Addr() {
		t.Fatalf("node = %q, want %q", resp.Node, node.Addr())
	}
	if exec.count() != 1 {
		t.Fatalf("calls = %d, want 1", exec.count())
	}
	if !node.S2SEnabled() || node.RootKeyFingerprint() != root.Fingerprint() {
		t.Fatal("节点应处于 S2S 模式且指纹一致")
	}
}

func TestSecureIdempotencyStillWorks(t *testing.T) {
	root := mustRootKey(t, 0x43)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)

	first, err := CallSecure(node.Addr(), root, Request{ID: "k-s2s", Instance: "I", Method: "run"}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CallSecure(node.Addr(), root, Request{ID: "k-s2s", Instance: "I", Method: "run"}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if exec.count() != 1 {
		t.Fatalf("同一幂等键跨两次加密连接只应执行一次，calls=%d", exec.count())
	}
	if first.Deduped || !second.Deduped {
		t.Fatalf("deduped 标记错误：first=%v second=%v", first.Deduped, second.Deduped)
	}
	if len(second.Outputs) != 1 || second.Outputs[0] != first.Outputs[0] {
		t.Fatalf("重复请求应复用结果：%v vs %v", first.Outputs, second.Outputs)
	}
}

func TestSecureWrongRootKey(t *testing.T) {
	serverRoot := mustRootKey(t, 0x44)
	clientRoot := mustRootKey(t, 0x45)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, serverRoot)

	_, err := CallSecure(node.Addr(), clientRoot, Request{Instance: "I", Method: "run"}, 2*time.Second)
	if err == nil {
		t.Fatal("错误 Root Key 的握手必须失败")
	}
	if !errors.Is(err, ErrS2SHandshake) {
		t.Fatalf("err = %v, want ErrS2SHandshake", err)
	}
	time.Sleep(50 * time.Millisecond)
	if exec.count() != 0 {
		t.Fatalf("握手失败不应执行 capability，calls=%d", exec.count())
	}
}

func TestSecureNodeRejectsPlainClient(t *testing.T) {
	root := mustRootKey(t, 0x46)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)

	if _, err := Call(node.Addr(), Request{Instance: "I", Method: "run"}, time.Second); err == nil {
		t.Fatal("明文客户端访问加密节点应失败")
	}
	time.Sleep(50 * time.Millisecond)
	if exec.count() != 0 {
		t.Fatalf("非法握手不应执行 capability，calls=%d", exec.count())
	}
}

func TestSecureClientRejectsPlainNode(t *testing.T) {
	root := mustRootKey(t, 0x47)
	node := startNode(t, &fakeExec{})

	_, err := CallSecure(node.Addr(), root, Request{Instance: "I", Method: "run"}, time.Second)
	if err == nil || !errors.Is(err, ErrS2SHandshake) {
		t.Fatalf("err = %v, want ErrS2SHandshake", err)
	}
}

func TestSecureConcurrentCalls(t *testing.T) {
	root := mustRootKey(t, 0x48)
	exec := &fakeExec{delay: 10 * time.Millisecond}
	node := startSecureNode(t, exec, root)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := CallSecure(node.Addr(), root, Request{
				ID: "c-" + string(rune('a'+i)), Instance: "I", Method: "run",
			}, 3*time.Second)
			if err == nil && !resp.OK {
				err = errors.New("resp not ok")
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if exec.count() != n {
		t.Fatalf("calls = %d, want %d", exec.count(), n)
	}
}

// ---------------- 异步握手状态机 ----------------

// A 方 BeginClientHandshake 必须立即返回；对端延迟 ack 时不能阻塞调用方。
func TestSecureAsyncHandshakeNonBlocking(t *testing.T) {
	root := mustRootKey(t, 0x49)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		defer conn.Close()
		time.Sleep(300 * time.Millisecond) // 故意推迟 ack
		_, err = ServerHandshake(conn, bufio.NewReader(conn), root, NewReplayCache(0), 2*time.Second)
		srvErr <- err
	}()

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	start := time.Now()
	hs := BeginClientHandshake(conn, br, root, 2*time.Second, nil)
	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Fatalf("BeginClientHandshake 阻塞了 %s，应 <150ms", elapsed)
	}
	if hs.State() == HandshakeEstablished {
		t.Fatal("对端延迟 ack 时不应已完成握手")
	}

	sess, err := hs.WaitTimeout(2 * time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if sess == nil || sess.Fingerprint() == "" {
		t.Fatal("应返回已建立会话")
	}
	if hs.State() != HandshakeEstablished {
		t.Fatalf("state = %s", hs.State())
	}
	if tr := hs.Transitions(); !containsState(tr, HandshakeHelloSent) || !containsState(tr, HandshakeEstablished) {
		t.Fatalf("状态迁移 = %v", tr)
	}
	if err := <-srvErr; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
}

func TestSecureAsyncHandshakeFailureState(t *testing.T) {
	serverRoot := mustRootKey(t, 0x4a)
	clientRoot := mustRootKey(t, 0x4b)
	node := startSecureNode(t, &fakeExec{}, serverRoot)

	conn, err := net.DialTimeout("tcp", node.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hs := BeginClientHandshake(conn, bufio.NewReader(conn), clientRoot, time.Second, nil)
	if _, err := hs.WaitTimeout(2 * time.Second); err == nil {
		t.Fatal("错误密钥应握手失败")
	}
	if hs.State() != HandshakeFailed {
		t.Fatalf("state = %s, want Failed", hs.State())
	}
	if !errors.Is(hs.Err(), ErrS2SHandshake) {
		t.Fatalf("Err = %v", hs.Err())
	}
}

// ---------------- 帧级安全：时间戳 / 篡改 / 跨连接重放 ----------------

func TestSecureStaleTimestampRejected(t *testing.T) {
	root := mustRootKey(t, 0x4c)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)
	conn, br, sess := dialSecureRaw(t, node.Addr(), root)

	payload, _ := json.Marshal(Request{Instance: "I", Method: "run"})
	if err := sess.writeFrameAt(conn, payload, time.Now().Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.ReadFrame(br, NewReplayCache(defaultReplayTTL)); err == nil {
		t.Fatal("10 秒前的帧应被拒绝")
	}
	time.Sleep(50 * time.Millisecond)
	if exec.count() != 0 {
		t.Fatalf("过期帧不应执行 capability，calls=%d", exec.count())
	}
}

func TestSecureTamperedFrameRejected(t *testing.T) {
	root := mustRootKey(t, 0x4d)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)
	conn, _, sess := dialSecureRaw(t, node.Addr(), root)

	payload, _ := json.Marshal(Request{Instance: "I", Method: "run"})
	f, err := sess.sealFrame(payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01 // 改 GCM tag
	f.Data = base64.StdEncoding.EncodeToString(raw)
	line, _ := json.Marshal(f)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("被篡改的帧应导致连接关闭")
	}
	time.Sleep(50 * time.Millisecond)
	if exec.count() != 0 {
		t.Fatalf("篡改帧不应执行 capability，calls=%d", exec.count())
	}
}

// 同一帧（nonce 相同）跨连接重放：握手是新的，但节点级 TTL Map 仍应拒绝。
func TestSecureReplayAcrossConnections(t *testing.T) {
	root := mustRootKey(t, 0x4e)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)

	conn1, br1, sess1 := dialSecureRaw(t, node.Addr(), root)
	payload, _ := json.Marshal(Request{Instance: "I", Method: "run"})
	f, err := sess1.sealFrame(payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(f)
	if _, err := conn1.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	respPayload, err := sess1.ReadFrame(br1, NewReplayCache(defaultReplayTTL))
	if err != nil {
		t.Fatalf("首次帧应成功: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(respPayload, &resp); err != nil || !resp.OK {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if exec.count() != 1 {
		t.Fatalf("calls = %d, want 1", exec.count())
	}

	conn2, _, _ := dialSecureRaw(t, node.Addr(), root)
	if _, err := conn2.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = conn2.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := conn2.Read(buf); err == nil {
		t.Fatal("重复 nonce 应导致连接关闭")
	}
	time.Sleep(50 * time.Millisecond)
	if exec.count() != 1 {
		t.Fatalf("重放帧不应再次执行，calls=%d", exec.count())
	}
}

// ---------------- Dispatcher 集成 ----------------

func TestSecureDispatcherAndFailover(t *testing.T) {
	root := mustRootKey(t, 0x4f)
	exec := &fakeExec{}
	node := startSecureNode(t, exec, root)

	d := &Dispatcher{
		Timeout: 2 * time.Second,
		Table:   NewTable(),
		RootKey: root,
		Log:     t.Logf,
	}
	d.Table.Set("I", []string{deadAddr(t), node.Addr()})

	resp, used, err := d.Dispatch("I", "run", []Value{{Kind: "int", I: 7}}, "d-1", "tr-d", 1)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !resp.OK || used != node.Addr() {
		t.Fatalf("resp=%+v used=%s", resp, used)
	}
	if exec.count() != 1 {
		t.Fatalf("calls = %d", exec.count())
	}

	// 错误 Root Key：所有节点都失败，返回 ErrAllNodesFailed。
	d.RootKey = mustRootKey(t, 0x50)
	if _, _, err := d.Dispatch("I", "run", nil, "d-2", "tr-d", 1); !errors.Is(err, ErrAllNodesFailed) {
		t.Fatalf("err = %v, want ErrAllNodesFailed", err)
	}
}
