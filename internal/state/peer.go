package state

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"neura/internal/cluster"
)

// S2SPeer 是 Peer 的唯一生产实现：所有跨节点状态 RPC 都走
// 预共享 RootKey 握手 → HKDF-SHA256 会话密钥 → AES-256-GCM 帧。
// 没有 RootKey 时直接失败（ErrPlaintext），不存在明文降级路径。
type S2SPeer struct {
	Self    string
	Root    *cluster.RootKey
	Timeout time.Duration
}

func NewS2SPeer(self string, root *cluster.RootKey, timeout time.Duration) *S2SPeer {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &S2SPeer{Self: self, Root: root, Timeout: timeout}
}

// Do 实现 Peer。
func (p *S2SPeer) Do(ctx context.Context, node string, rpc RPC) (Reply, error) {
	if p.Root == nil {
		return Reply{}, ErrPlaintext
	}
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	payload, err := json.Marshal(rpc)
	if err != nil {
		return Reply{}, fmt.Errorf("state: encode rpc: %w", err)
	}
	timeout := p.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if remain := time.Until(dl); remain < timeout {
			timeout = remain
		}
	}
	if timeout <= 0 {
		return Reply{}, context.DeadlineExceeded
	}
	resp, err := cluster.CallSecure(node, p.Root, cluster.Request{
		TraceID: rpc.TraceID,
		Op:      "state." + rpc.Kind,
		Payload: payload,
		From:    p.Self,
	}, timeout)
	if err != nil {
		return Reply{}, err
	}
	if !resp.OK {
		return Reply{}, fmt.Errorf("state rpc %s@%s: %s", rpc.Kind, node, resp.Error)
	}
	var rep Reply
	if err := json.Unmarshal(resp.Blob, &rep); err != nil {
		return Reply{}, fmt.Errorf("state: decode reply: %w", err)
	}
	return rep, nil
}

// Install 把副本挂到 S2S 节点上。调用方仍需 node.EnableS2S(root) 才会接受连接。
func (r *Replica) Install(n *cluster.Node) {
	n.SetOpsHandler(r.Handle)
}
