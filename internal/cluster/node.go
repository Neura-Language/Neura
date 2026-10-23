package cluster

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

// Executor 是节点上的执行器（由 runtime.Program 实现）。
// traceID/depth 随请求传入，用于把远端 trace 片段串到发起端。
type Executor interface {
	CallCapability(instance, method string, args []Value, traceID string, depth int) (ExecResult, error)
}

// Swappable 是可热替换的执行器：Swap 原子换掉底层 Program，监听连接与在途请求不受影响。
type Swappable struct {
	mu  sync.RWMutex
	cur Executor
}

func NewSwappable(e Executor) *Swappable { return &Swappable{cur: e} }

func (s *Swappable) Swap(e Executor) {
	s.mu.Lock()
	s.cur = e
	s.mu.Unlock()
}

func (s *Swappable) CallCapability(instance, method string, args []Value, traceID string, depth int) (ExecResult, error) {
	s.mu.RLock()
	e := s.cur
	s.mu.RUnlock()
	return e.CallCapability(instance, method, args, traceID, depth)
}

// Node 是 TCP 服务端：每条连接处理一条 JSON 请求，返回一条 JSON 响应。
// 所有请求先过幂等表：同一 ID 只执行一次，重复请求直接复用结果并标 deduped。
//
// 配置 EnableS2S(root) 后，节点要求每条连接先做加密握手，业务帧必须 AES-256-GCM
// 加密且携带 3 秒内的时间戳；未配置时保持原有明文 JSON-over-TCP 行为。
type Node struct {
	configured string
	ln         net.Listener
	exec       Executor
	idem       *Idempotency
	logf       func(string, ...any)

	mu    sync.Mutex
	conns map[net.Conn]struct{}

	// 控制面 RPC（state 副本）处理器。为 nil 时拒绝 Op 请求。
	opsMu sync.RWMutex
	ops   OpsHandler

	// S2S（EnableS2S 之前设置；handle 里用读锁取快照）
	s2sMu            sync.RWMutex
	root             *RootKey
	replay           *ReplayCache
	handshakeTimeout time.Duration
	frameTimeout     time.Duration
}

func NewNode(addr string, exec Executor, logf func(string, ...any)) *Node {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Node{
		configured:       addr,
		exec:             exec,
		idem:             NewIdempotency(1024),
		logf:             logf,
		conns:            map[net.Conn]struct{}{},
		handshakeTimeout: 5 * time.Second,
		frameTimeout:     30 * time.Second,
	}
}

// EnableS2S 开启 S2S 加密：必须提供线下分发的 Root Key。
// 可重复调用（例如热更新密钥后），新连接立即使用新配置，在途连接不受影响。
func (n *Node) EnableS2S(root *RootKey) {
	n.s2sMu.Lock()
	defer n.s2sMu.Unlock()
	n.root = root
	if n.replay == nil {
		// 进程内 TTL Map 代替 Redis：默认保留 2 倍时间戳窗口。
		n.replay = NewReplayCache(defaultReplayTTL)
	}
}

// SetReplayCache 替换防重放表（测试可注入更短 TTL / 可控时钟）。
func (n *Node) SetReplayCache(c *ReplayCache) {
	n.s2sMu.Lock()
	n.replay = c
	n.s2sMu.Unlock()
}

// S2SEnabled 报告节点是否要求加密。
func (n *Node) S2SEnabled() bool {
	n.s2sMu.RLock()
	defer n.s2sMu.RUnlock()
	return n.root != nil
}

// RootKeyFingerprint 返回可安全打印的 Root Key 指纹；未启用时为空。
func (n *Node) RootKeyFingerprint() string {
	n.s2sMu.RLock()
	defer n.s2sMu.RUnlock()
	if n.root == nil {
		return ""
	}
	return n.root.Fingerprint()
}

func (n *Node) s2sConfig() (*RootKey, *ReplayCache, time.Duration, time.Duration) {
	n.s2sMu.RLock()
	defer n.s2sMu.RUnlock()
	return n.root, n.replay, n.handshakeTimeout, n.frameTimeout
}

// Listen 绑定端口（addr 可用 ":0" 取随机端口，测试用）。
func (n *Node) Listen() error {
	ln, err := net.Listen("tcp", n.configured)
	if err != nil {
		return err
	}
	n.ln = ln
	return nil
}

// Addr 返回实际监听地址。
func (n *Node) Addr() string {
	if n.ln == nil {
		return n.configured
	}
	return n.ln.Addr().String()
}

// Serve 接受连接直到 Close。
func (n *Node) Serve() {
	for {
		conn, err := n.ln.Accept()
		if err != nil {
			return
		}
		n.mu.Lock()
		n.conns[conn] = struct{}{}
		n.mu.Unlock()
		go n.handle(conn)
	}
}

func (n *Node) Close() error {
	if n.ln == nil {
		return nil
	}
	err := n.ln.Close()
	n.mu.Lock()
	for c := range n.conns {
		_ = c.Close()
	}
	n.mu.Unlock()
	return err
}

func (n *Node) handle(conn net.Conn) {
	defer func() {
		n.mu.Lock()
		delete(n.conns, conn)
		n.mu.Unlock()
		_ = conn.Close()
	}()

	br := bufio.NewReader(conn)
	root, replay, hsTimeout, frameTimeout := n.s2sConfig()

	var req Request
	var sess *Session
	if root != nil {
		// 加密模式：先握手（B 方），再读一帧加密 Request。
		s, err := ServerHandshake(conn, br, root, replay, hsTimeout)
		if err != nil {
			// 未认证连接失败时只记录日志、直接关闭，不回明文错误，避免探测。
			n.logf("s2s handshake from %s: %v", conn.RemoteAddr(), err)
			return
		}
		sess = s
		if err := conn.SetDeadline(time.Now().Add(frameTimeout)); err != nil {
			n.logf("s2s %s: set deadline: %v", conn.RemoteAddr(), err)
			return
		}
		payload, err := sess.ReadFrame(br, replay)
		if err != nil {
			n.logf("s2s read from %s: %v", conn.RemoteAddr(), err)
			return
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			n.writeSecure(conn, sess, Response{OK: false, Error: "bad request: " + err.Error(), Node: n.Addr()})
			return
		}
	} else {
		// 兼容模式：原有明文 JSON-over-TCP。
		if err := json.NewDecoder(br).Decode(&req); err != nil {
			_ = json.NewEncoder(conn).Encode(Response{OK: false, Error: "bad request: " + err.Error(), Node: n.Addr()})
			return
		}
	}

	resp := n.execute(req)
	if sess != nil {
		n.writeSecure(conn, sess, resp)
		return
	}
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		n.logf("node %s: write response: %v", n.Addr(), err)
	}
}

// OpsHandler 处理控制面 RPC：payload 由上层定义（state 包用 JSON RPC）。
// 它永远只在 S2S 握手成功后被调用；EnableS2S 之前 Node 不会接受外部连接。
type OpsHandler func(payload []byte) ([]byte, error)

// SetOpsHandler 安装控制面处理器（可在 Serve 前/后调用）。
func (n *Node) SetOpsHandler(h OpsHandler) {
	n.opsMu.Lock()
	n.ops = h
	n.opsMu.Unlock()
}

// execute 跑幂等表 + 执行器，并统一补 ID/Node、打日志。
func (n *Node) execute(req Request) Response {
	if req.Op != "" {
		return n.executeOps(req)
	}
	call := func() Response {
		if n.exec == nil {
			return Response{ID: req.ID, TraceID: req.TraceID, OK: false, Error: "executor not installed", Node: n.Addr()}
		}
		res, err := n.exec.CallCapability(req.Instance, req.Method, req.Args, req.TraceID, req.Depth)
		if err != nil {
			return Response{ID: req.ID, TraceID: req.TraceID, OK: false, Error: err.Error(), Node: n.Addr()}
		}
		return Response{
			ID:      req.ID,
			TraceID: req.TraceID,
			OK:      true,
			Outputs: res.Outputs,
			Learns:  res.Learns,
			Trace:   res.Trace,
			Node:    n.Addr(),
		}
	}

	var resp Response
	if req.ID == "" {
		resp = call() // 无幂等键：直接执行
	} else {
		resp, _ = n.idem.Do(req.ID, call)
	}
	resp.ID = req.ID
	resp.Node = n.Addr()
	n.logf("rpc %s.%s id=%s trace=%s depth=%d from=%s ok=%v deduped=%v trace_records=%s",
		req.Instance, req.Method, req.ID, shortID(req.TraceID), req.Depth, req.From, resp.OK, resp.Deduped, traceCount(resp.Trace))
	return resp
}

func (n *Node) executeOps(req Request) Response {
	resp := Response{ID: req.ID, TraceID: req.TraceID, Node: n.Addr()}
	// 控制面 RPC 绝不走明文：未启用 S2S 的节点直接拒绝。
	if !n.S2SEnabled() {
		resp.Error = "control-plane RPC requires S2S"
		n.logf("ops %s from=%s rejected: S2S disabled", req.Op, req.From)
		return resp
	}
	n.opsMu.RLock()
	h := n.ops
	n.opsMu.RUnlock()
	if h == nil {
		resp.Error = "ops handler not installed"
		n.logf("ops %s from=%s: handler not installed", req.Op, req.From)
		return resp
	}
	blob, err := h(req.Payload)
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.OK = true
		resp.Blob = blob
	}
	n.logf("ops %s id=%s trace=%s from=%s ok=%v", req.Op, shortID(req.ID), shortID(req.TraceID), req.From, resp.OK)
	return resp
}

func (n *Node) writeSecure(conn net.Conn, sess *Session, resp Response) {
	payload, err := json.Marshal(resp)
	if err != nil {
		n.logf("node %s: encode s2s response: %v", n.Addr(), err)
		return
	}
	_, _, _, frameTimeout := n.s2sConfig()
	if err := conn.SetWriteDeadline(time.Now().Add(frameTimeout)); err != nil {
		n.logf("node %s: s2s set write deadline: %v", n.Addr(), err)
		return
	}
	if err := sess.WriteFrame(conn, payload); err != nil {
		n.logf("node %s: s2s write response: %v", n.Addr(), err)
	}
}

// ---------------- 幂等表 ----------------

type idemEntry struct {
	done chan struct{}
	resp Response
	ok   bool
}

// Idempotency 是节点本地的幂等表：同一 key 只执行一次，并发重复请求会等第一次完成。
type Idempotency struct {
	mu    sync.Mutex
	m     map[string]*idemEntry
	order []string
	cap   int
}

func NewIdempotency(capacity int) *Idempotency {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Idempotency{m: map[string]*idemEntry{}, cap: capacity}
}

// Do 执行 fn（或复用已有结果）。返回值 deduped 表示命中幂等表。
func (t *Idempotency) Do(key string, fn func() Response) (Response, bool) {
	t.mu.Lock()
	if e, ok := t.m[key]; ok {
		t.mu.Unlock()
		<-e.done
		resp := e.resp
		resp.Deduped = true
		return resp, true
	}
	e := &idemEntry{done: make(chan struct{})}
	t.m[key] = e
	t.order = append(t.order, key)
	t.mu.Unlock()

	resp := fn()

	t.mu.Lock()
	e.resp = resp
	close(e.done)
	t.evictLocked()
	t.mu.Unlock()
	return resp, false
}

func (t *Idempotency) evictLocked() {
	for len(t.order) > t.cap {
		oldest := t.order[0]
		t.order = t.order[1:]
		if e, ok := t.m[oldest]; ok {
			select {
			case <-e.done:
				delete(t.m, oldest)
			default:
				// 还在执行中，放回去，下轮再试
				t.order = append(t.order, oldest)
				return
			}
		}
	}
}

// Len 是当前幂等表条目数（测试/观测用）。
func (t *Idempotency) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}

// ---------------- 辅助 ----------------

func shortID(id string) string {
	if id == "" {
		return "-"
	}
	if len(id) > 16 {
		return id[:16]
	}
	return id
}

// traceCount 只用于日志：数一下不透明 trace 片段里的记录条数。
func traceCount(raw []byte) string {
	if len(raw) == 0 {
		return "0"
	}
	var recs []json.RawMessage
	if err := json.Unmarshal(raw, &recs); err != nil {
		return "?"
	}
	return fmt.Sprintf("%d", len(recs))
}

// FormatRequest 便于日志/测试输出。
func FormatRequest(r Request) string {
	return fmt.Sprintf("%s.%s(id=%s,args=%d)", r.Instance, r.Method, r.ID, len(r.Args))
}
