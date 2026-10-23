p = 'internal/cluster/node.go'
s = open(p).read()

s = s.replace('''// Executor 是节点上的执行器（由 runtime.Program 实现）。
type Executor interface {
	CallCapability(instance, method string, args []Value) (outputs, learns []string, err error)
}''', '''// Executor 是节点上的执行器（由 runtime.Program 实现）。
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
}''')

s = s.replace('''	call := func() Response {
		outputs, learns, err := n.exec.CallCapability(req.Instance, req.Method, req.Args)
		if err != nil {
			return Response{ID: req.ID, OK: false, Error: err.Error(), Node: n.Addr()}
		}
		return Response{ID: req.ID, OK: true, Outputs: outputs, Learns: learns, Node: n.Addr()}
	}''', '''	call := func() Response {
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
	}''')

s = s.replace('''	n.logf("rpc %s.%s id=%s from=%s ok=%v deduped=%v", req.Instance, req.Method, req.ID, req.From, resp.OK, resp.Deduped)''',
'''	n.logf("rpc %s.%s id=%s trace=%s depth=%d from=%s ok=%v deduped=%v trace_records=%s",
		req.Instance, req.Method, req.ID, shortID(req.TraceID), req.Depth, req.From, resp.OK, resp.Deduped, traceCount(resp.Trace))''')

s = s.replace('''// ---------------- 辅助 ----------------''', '''// ---------------- 辅助 ----------------

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
}''')

open(p, 'w').write(s)
print('node.go patched')

p = 'internal/cluster/table.go'
s = open(p).read()
s = s.replace('''func (d *Dispatcher) Dispatch(instance, method string, args []Value, id string) (Response, string, error) {''',
'''func (d *Dispatcher) Dispatch(instance, method string, args []Value, id, traceID string, depth int) (Response, string, error) {''')
s = s.replace('''		resp, err := Call(node, Request{
			ID:       id,
			Instance: instance,
			Method:   method,
			Args:     args,
			From:     d.Self,
		}, d.Timeout)''', '''		resp, err := Call(node, Request{
			ID:       id,
			TraceID:  traceID,
			Depth:    depth,
			Instance: instance,
			Method:   method,
			Args:     args,
			From:     d.Self,
		}, d.Timeout)''')
open(p, 'w').write(s)
print('table.go patched')
