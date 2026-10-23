// Package cluster 是 Neura 的跨节点传输层（Phase 5）。
//
// 协议：标准库 JSON over TCP，一条请求一行 JSON、一条响应一行 JSON，零外部依赖。
// Phase 5.1：请求携带 trace_id / depth，响应回传远端产生的 trace 片段（不透明 JSON），
// 由发起端聚合；节点表支持 SIGHUP 热加载（Swappable executor，不断开连接）。
package cluster

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Value 是 wire 上的值。instance 只传名字，接收端在自己库里解析。
type Value struct {
	Kind string  `json:"kind"`
	I    int64   `json:"i,omitempty"`
	F    float64 `json:"f,omitempty"`
	B    bool    `json:"b,omitempty"`
	S    string  `json:"s,omitempty"`
}

// Request 是一次远程 capability 调用。
// ID 是幂等键（空表示不去重）；TraceID/Depth 负责跨节点 trace 串联。
type Request struct {
	ID       string  `json:"id,omitempty"`
	TraceID  string  `json:"trace_id,omitempty"`
	Depth    int     `json:"depth,omitempty"`
	Instance string  `json:"instance"`
	Method   string  `json:"method"`
	Args     []Value `json:"args,omitempty"`
	From     string  `json:"from,omitempty"`

	// Op/Payload 是节点级控制面 RPC（state 副本的 reserve/commit/abort）。
	// 与业务 capability 共用同一条 S2S 加密帧，不允许另开明文通道。
	Op      string `json:"op,omitempty"`
	Payload []byte `json:"payload,omitempty"`
}

// ExecResult 是节点内部执行结果（Trace 是不透明的 JSON 片段，由 runtime 序列化）。
type ExecResult struct {
	Outputs []string
	Learns  []string
	Trace   json.RawMessage
}

// Response 是远程调用结果。Outputs/Learns 与本地执行同格式；Trace 由发起端聚合。
type Response struct {
	ID      string          `json:"id,omitempty"`
	TraceID string          `json:"trace_id,omitempty"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Outputs []string        `json:"outputs,omitempty"`
	Learns  []string        `json:"learns,omitempty"`
	Trace   json.RawMessage `json:"trace,omitempty"`
	Deduped bool            `json:"deduped,omitempty"`
	Node    string          `json:"node,omitempty"`

	// Blob 是控制面 RPC 的应答载荷（base64 由 encoding/json 处理）。
	Blob []byte `json:"blob,omitempty"`

	// Local 只在本地短路时使用，不上线。
	Local bool `json:"-"`
}

// Call 向 addr 发一次请求（一次连接一条请求，简单可预测）。
func Call(addr string, req Request, timeout time.Duration) (Response, error) {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return Response{}, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, err
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("send: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("recv: %w", err)
	}
	return resp, nil
}
