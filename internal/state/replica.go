package state

import (
	"encoding/json"
	"fmt"
	"time"
)

// RPC 是副本间的状态操作。Kind 取值见 RPC* 常量。
type RPC struct {
	Kind    string        `json:"kind"`
	TxID    string        `json:"tx_id,omitempty"`
	TraceID string        `json:"trace_id,omitempty"`
	Writes  []Write       `json:"writes,omitempty"`
	Keys    []string      `json:"keys,omitempty"`
	TTL     time.Duration `json:"ttl,omitempty"`
}

const (
	RPCReserve = "reserve"
	RPCCommit  = "commit"
	RPCAbort   = "abort"
	RPCApplied = "applied"
	RPCGet     = "get"
)

// Reply 是副本应答。错误跨线时用 Error/Conflict 两个显式字段，
// 避免把 Go error 字符串当成协议语义。
type Reply struct {
	Records  []Record       `json:"records,omitempty"`
	Applied  bool           `json:"applied,omitempty"`
	Error    string         `json:"error,omitempty"`
	Conflict *ConflictError `json:"conflict,omitempty"`
}

// Err 把 wire 错误还原成 Go error。
func (r Reply) Err() error {
	if r.Conflict != nil {
		return r.Conflict
	}
	if r.Error != "" {
		return fmt.Errorf("state rpc: %s", r.Error)
	}
	return nil
}

// Replica 把 Store 暴露成 RPC handler。Node.SetOpsHandler(rep.Handle) 即可挂到 S2S 之后。
type Replica struct {
	Store *Store
}

func NewReplica(s *Store) *Replica { return &Replica{Store: s} }

// Handle 是 cluster.OpsHandler 的实现：payload 必须是 JSON RPC。
func (r *Replica) Handle(payload []byte) ([]byte, error) {
	var rpc RPC
	if err := json.Unmarshal(payload, &rpc); err != nil {
		return nil, fmt.Errorf("state: bad rpc payload: %w", err)
	}
	rep, err := r.HandleRPC(rpc)
	if err != nil {
		// 协议级错误也走结构化 Reply，方便对端区分冲突与网络故障。
		if ce, ok := err.(*ConflictError); ok {
			return json.Marshal(Reply{Conflict: ce})
		}
		return json.Marshal(Reply{Error: err.Error()})
	}
	return json.Marshal(rep)
}

// HandleRPC 是本地/远端共用的唯一入口。
func (r *Replica) HandleRPC(rpc RPC) (Reply, error) {
	switch rpc.Kind {
	case RPCReserve:
		if _, err := r.Store.Reserve(rpc.TxID, rpc.Writes, rpc.TTL); err != nil {
			return Reply{}, err
		}
		return Reply{}, nil
	case RPCCommit:
		res, applied, err := r.Store.Commit(rpc.TxID, rpc.TraceID, rpc.Keys)
		if err != nil {
			return Reply{}, err
		}
		return Reply{Records: recordsOf(res, rpc.Keys), Applied: applied}, nil
	case RPCAbort:
		r.Store.Abort(rpc.TxID, rpc.Keys)
		return Reply{}, nil
	case RPCApplied:
		res, ok := r.Store.Applied(rpc.TxID)
		if !ok {
			return Reply{Applied: false}, nil
		}
		return Reply{Records: recordsOf(res, rpc.Keys), Applied: true}, nil
	case RPCGet:
		return Reply{Records: r.Store.Snapshot(rpc.Keys)}, nil
	default:
		return Reply{}, fmt.Errorf("state: unknown rpc kind %q", rpc.Kind)
	}
}

func recordsOf(res *TxnResult, keys []string) []Record {
	if res == nil {
		return nil
	}
	out := make([]Record, 0, len(keys))
	for _, k := range keys {
		if rec, ok := res.Records[k]; ok {
			out = append(out, rec)
		}
	}
	return out
}
