package state

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Version 是每键单调版本号。0 表示键不存在。
type Version uint64

// Write 是一次 CAS 写意图。Expect=0 表示「仅当键不存在时创建」。
type Write struct {
	Key    string  `json:"key"`
	Value  []byte  `json:"value,omitempty"`
	Expect Version `json:"expect"`
	Delete bool    `json:"delete,omitempty"`
}

// Record 是已提交的版本化记录。Value 是业务层不透明字节（JSON/MsgPack 均可）。
type Record struct {
	Key     string  `json:"key"`
	Version Version `json:"version"`
	Value   []byte  `json:"value,omitempty"`
	Deleted bool    `json:"deleted,omitempty"`
	TraceID string  `json:"trace_id,omitempty"`
	TxID    string  `json:"tx_id,omitempty"`
}

// TxnResult 是一个事务在某个副本上的提交结果；也是幂等窗口的缓存值。
type TxnResult struct {
	TxID    string            `json:"tx_id"`
	TraceID string            `json:"trace_id"`
	Records map[string]Record `json:"records"`
}

// 事务错误。
var (
	ErrConflict    = errors.New("state: version conflict")
	ErrReserved    = errors.New("state: key reserved by another txn")
	ErrNotReserved = errors.New("state: key not reserved by this txn")
	ErrNoTxn       = errors.New("state: transaction has no writes")
	ErrPlaintext   = errors.New("state: cross-node state RPC requires S2S root key")
)

// ConflictError 带当前版本，调用方可据此重建事务。
type ConflictError struct {
	Key      string
	Expected Version
	Current  Version
	Owner    string
}

func (e *ConflictError) Error() string {
	if e.Owner != "" {
		return fmt.Sprintf("%v: %s expected=%d current=%d owner=%s", ErrConflict, e.Key, e.Expected, e.Current, e.Owner)
	}
	return fmt.Sprintf("%v: %s expected=%d current=%d", ErrConflict, e.Key, e.Expected, e.Current)
}

// Is 让 errors.Is(err, ErrConflict) 成立。
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// Store 是单节点内存副本：CAS + 预留（reservation）+ 提交幂等窗口。
//
// 提交分两步：
//  1. Reserve：校验 Expect，写入 pending 并把 key 租给 txID（TTL 可回收）。
//  2. Commit：把 pending 原子应用，版本 +1，并把结果写进 txID 幂等窗口。
//
// 同一 txID 重复 Reserve/Commit 是幂等的；不同 txID 在租约期内互斥。
type Store struct {
	mu      sync.Mutex
	kv      map[string]*entry
	applied *Window
	now     func() time.Time
}

type entry struct {
	rec     Record
	pending *Write
	by      string
	until   time.Time
}

// NewStore 创建副本。窗口容量/TTL 决定 exactly-once 的保留边界。
func NewStore(cap int, ttl time.Duration) *Store {
	return &Store{
		kv:      map[string]*entry{},
		applied: NewWindow(cap, ttl),
		now:     time.Now,
	}
}

// SetClock 注入可控时钟（确定性测试）。
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
	s.applied.SetClock(now)
}

// Reserve 批量预留。校验全部通过后才写入 pending（节点内原子）。
func (s *Store) Reserve(txID string, writes []Write, ttl time.Duration) ([]Reservation, error) {
	if txID == "" || len(writes) == 0 {
		return nil, ErrNoTxn
	}
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 先做整体校验，避免部分预留。
	now := s.now()
	for _, w := range writes {
		if w.Key == "" {
			return nil, fmt.Errorf("state: empty key")
		}
		e := s.entryLocked(w.Key)
		if e.by != "" && e.by != txID && now.Before(e.until) {
			return nil, &ConflictError{Key: w.Key, Expected: w.Expect, Current: e.rec.Version, Owner: e.by}
		}
		if e.by == txID {
			// 同一事务重复预留：只校验 pending 一致，保持幂等。
			if e.pending != nil && !sameWrite(*e.pending, w) {
				return nil, fmt.Errorf("state: tx %s re-reserve %s with different write", txID, w.Key)
			}
			continue
		}
		// 已提交过该 txID（重试路径）：允许直接返回，Commit 会命中 applied 窗口。
		if res, ok := s.appliedLocked(txID); ok {
			if r, ok := res.Records[w.Key]; ok {
				_ = r
				continue
			}
		}
		if e.rec.Version != w.Expect {
			return nil, &ConflictError{Key: w.Key, Expected: w.Expect, Current: e.rec.Version}
		}
	}

	out := make([]Reservation, 0, len(writes))
	appliedRes, appliedOK := s.appliedLocked(txID)
	for _, w := range writes {
		if appliedOK {
			if _, done := appliedRes.Records[w.Key]; done {
				out = append(out, Reservation{Key: w.Key, Expect: w.Expect, TxID: txID})
				continue
			}
		}
		e := s.entryLocked(w.Key)
		if e.by != txID {
			e.by, e.until = txID, now.Add(ttl)
			pw := w
			e.pending = &pw
		} else if e.pending == nil {
			pw := w
			e.pending = &pw
			e.until = now.Add(ttl)
		}
		out = append(out, Reservation{Key: w.Key, Expect: w.Expect, TxID: txID})
	}
	return out, nil
}

// Reservation 是预留回执。
type Reservation struct {
	Key    string  `json:"key"`
	Expect Version `json:"expect"`
	TxID   string  `json:"tx_id"`
}

// Commit 原子应用所有预留；同一 txID 重复提交返回缓存结果。
func (s *Store) Commit(txID, traceID string, keys []string) (*TxnResult, bool, error) {
	if txID == "" || len(keys) == 0 {
		return nil, false, ErrNoTxn
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if res, ok := s.appliedLocked(txID); ok {
		return res, true, nil
	}
	for _, k := range keys {
		e := s.entryLocked(k)
		if e.by != txID || e.pending == nil {
			return nil, false, fmt.Errorf("%w: %s tx=%s", ErrNotReserved, k, txID)
		}
	}
	res := &TxnResult{TxID: txID, TraceID: traceID, Records: make(map[string]Record, len(keys))}
	for _, k := range keys {
		e := s.entryLocked(k)
		w := *e.pending
		rec := Record{
			Key:     k,
			Version: e.rec.Version + 1,
			Value:   append([]byte(nil), w.Value...),
			Deleted: w.Delete,
			TraceID: traceID,
			TxID:    txID,
		}
		e.rec = rec
		e.pending = nil
		e.by = ""
		e.until = time.Time{}
		if rec.Deleted {
			rec.Value = nil
		}
		res.Records[k] = rec
	}
	s.applied.Put(txID, res)
	return res, false, nil
}

// Abort 释放预留。可安全重复调用。
func (s *Store) Abort(txID string, keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if e := s.kv[k]; e != nil && e.by == txID {
			e.pending = nil
			e.by = ""
			e.until = time.Time{}
		}
	}
}

// Applied 查询 txID 是否已提交（跨节点重试短路）。
func (s *Store) Applied(txID string) (*TxnResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appliedLocked(txID)
}

func (s *Store) appliedLocked(txID string) (*TxnResult, bool) {
	v, ok := s.applied.Get(txID)
	if !ok {
		return nil, false
	}
	res, ok := v.(*TxnResult)
	return res, ok
}

// Get 返回已提交记录（不返回 pending）。
func (s *Store) Get(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.kv[key]
	if e == nil || e.rec.Version == 0 {
		return Record{}, false
	}
	r := e.rec
	r.Value = append([]byte(nil), r.Value...)
	return r, true
}

// Snapshot 读取一组键。
func (s *Store) Snapshot(keys []string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(keys))
	for _, k := range keys {
		if e := s.kv[k]; e != nil && e.rec.Version != 0 {
			r := e.rec
			r.Value = append([]byte(nil), r.Value...)
			out = append(out, r)
		}
	}
	return out
}

// Merge 是反熵入口：按 (Version, TxID) 做确定性 LWW。
// 只在副本持有者失联后由修复任务调用；正常写路径不走它。
func (s *Store) Merge(records []Record) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range records {
		if r.Key == "" || r.Version == 0 {
			continue
		}
		e := s.entryLocked(r.Key)
		cur := e.rec
		if r.Version > cur.Version || (r.Version == cur.Version && r.TxID > cur.TxID) {
			r.Value = append([]byte(nil), r.Value...)
			e.rec = r
			n++
		}
	}
	return n
}

// Keys 返回已提交键的升序列表（观测用）。
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.kv))
	for k, e := range s.kv {
		if e.rec.Version != 0 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Len 是已提交键数量。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.kv {
		if e.rec.Version != 0 {
			n++
		}
	}
	return n
}

func (s *Store) entryLocked(key string) *entry {
	e := s.kv[key]
	if e == nil {
		e = &entry{rec: Record{Key: key}}
		s.kv[key] = e
	}
	return e
}

func sameWrite(a, b Write) bool {
	if a.Key != b.Key || a.Expect != b.Expect || a.Delete != b.Delete || len(a.Value) != len(b.Value) {
		return false
	}
	for i := range a.Value {
		if a.Value[i] != b.Value[i] {
			return false
		}
	}
	return true
}
