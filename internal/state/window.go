package state

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// Window 是 TTL + 容量双限的幂等窗口（进程内，无 Redis）。
//
// 语义：
//   - 同一 key 的 Do 只会执行一次；并发调用等待第一次结果（singleflight）。
//   - 成功结果被缓存到 TTL/容量淘汰；失败不缓存，允许调用方用同一 key 重试。
//   - 所有入口都做惰性过期，不依赖后台 goroutine。
type Window struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	now   func() time.Time
	items map[string]*list.Element
	order *list.List // front = newest
}

type winEntry struct {
	key  string
	val  any
	err  error
	at   time.Time
	done chan struct{}
	busy bool
}

// NewWindow 创建幂等窗口。cap<=0 默认 4096；ttl<=0 默认 5 分钟。
func NewWindow(cap int, ttl time.Duration) *Window {
	if cap <= 0 {
		cap = 4096
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Window{
		cap:   cap,
		ttl:   ttl,
		now:   time.Now,
		items: map[string]*list.Element{},
		order: list.New(),
	}
}

// SetClock 注入可控时钟（测试/确定性重放）。
func (w *Window) SetClock(now func() time.Time) {
	w.mu.Lock()
	w.now = now
	w.mu.Unlock()
}

// Get 读取未过期结果。
func (w *Window) Get(key string) (any, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	el := w.items[key]
	if el == nil {
		return nil, false
	}
	e := el.Value.(*winEntry)
	if e.busy || w.expiredLocked(e) {
		if !e.busy {
			w.removeLocked(el)
		}
		return nil, false
	}
	w.order.MoveToFront(el)
	return e.val, true
}

// Put 写入成功结果。
func (w *Window) Put(key string, val any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	el := w.items[key]
	if el != nil {
		e := el.Value.(*winEntry)
		if e.busy {
			return
		}
		e.val, e.at = val, w.now()
		w.order.MoveToFront(el)
	} else {
		el = w.order.PushFront(&winEntry{key: key, val: val, at: w.now()})
		w.items[key] = el
	}
	w.evictLocked()
}

// Do 是幂等执行：命中缓存直接返回 (value,true,nil)；并发同 key 只执行一次。
func (w *Window) Do(key string, fn func() (any, error)) (any, bool, error) {
	w.mu.Lock()
	if el := w.items[key]; el != nil {
		e := el.Value.(*winEntry)
		if e.busy {
			done := e.done
			w.mu.Unlock()
			<-done
			return e.val, true, e.err
		}
		if !w.expiredLocked(e) {
			w.order.MoveToFront(el)
			w.mu.Unlock()
			return e.val, true, nil
		}
		w.removeLocked(el)
	}
	e := &winEntry{key: key, at: w.now(), done: make(chan struct{}), busy: true}
	w.items[key] = w.order.PushFront(e)
	w.mu.Unlock()

	v, err := safeDo(fn)

	w.mu.Lock()
	e.val, e.err, e.busy = v, err, false
	close(e.done)
	if err != nil {
		// 失败不缓存：删除入口，允许用同一 key 重试。
		if el := w.items[key]; el != nil && el.Value.(*winEntry) == e {
			w.removeLocked(el)
		}
	} else {
		e.at = w.now()
		w.evictLocked()
	}
	w.mu.Unlock()
	return v, false, err
}

func safeDo(fn func() (any, error)) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("state: window fn panic: %v", r)
		}
	}()
	return fn()
}

func (w *Window) expiredLocked(e *winEntry) bool {
	return !e.at.IsZero() && w.now().Sub(e.at) > w.ttl
}

func (w *Window) removeLocked(el *list.Element) {
	e := el.Value.(*winEntry)
	delete(w.items, e.key)
	w.order.Remove(el)
}

// evictLocked 从尾部淘汰过期/超容条目。busy 条目跳过。
func (w *Window) evictLocked() {
	for len(w.items) > w.cap || w.hasExpiredLocked() {
		var victim *list.Element
		for el := w.order.Back(); el != nil; el = el.Prev() {
			e := el.Value.(*winEntry)
			if e.busy {
				continue
			}
			if len(w.items) > w.cap || w.expiredLocked(e) {
				victim = el
				break
			}
		}
		if victim == nil {
			return
		}
		w.removeLocked(victim)
	}
}

func (w *Window) hasExpiredLocked() bool {
	el := w.order.Back()
	if el == nil {
		return false
	}
	e := el.Value.(*winEntry)
	return !e.busy && w.expiredLocked(e)
}

// Len 是当前条目数。
func (w *Window) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.items)
}
