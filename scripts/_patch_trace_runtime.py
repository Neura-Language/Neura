import re

p = 'internal/runtime/interp.go'
s = open(p).read()

# 1) TraceRecord 增加跨节点字段
s = s.replace('''type TraceRecord struct {
	Seq        int         `json:"seq"`
	Subject    string      `json:"subject"`''', '''type TraceRecord struct {
	TraceID    string      `json:"trace_id,omitempty"`
	Node       string      `json:"node,omitempty"`
	Depth      int         `json:"depth,omitempty"`
	Local      int         `json:"local,omitempty"`
	Seq        int         `json:"seq"`
	Subject    string      `json:"subject"`''')

# 2) Program 增加 selfAddr / localSeq
s = s.replace('''	reqSeq  int64
	nodeTag string
}''', '''	reqSeq  int64
	nodeTag string
	selfAddr string
	localSeq int
}''')

# 3) Result 增加 TraceID
s = s.replace('''// Result 是一次运行的产物。
type Result struct {
	Returned Value''', '''// Result 是一次运行的产物。TraceID 是全局 trace 串联键。
type Result struct {
	TraceID  string
	Returned Value''')

# 4) 删掉旧的 Program.addTrace，改为 interp.addTrace + 聚合
s = s.replace('''func (p *Program) addTrace(r TraceRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	r.Seq = p.seq
	p.trace = append(p.trace, r)
}''', '''// interp.addTrace 打上 trace_id / node / depth / local 序号，再进全局序列。
func (in *interp) addTrace(r TraceRecord) {
	r.TraceID = in.traceID
	r.Node = in.p.selfAddr
	r.Depth = in.depth
	p := in.p
	p.mu.Lock()
	p.localSeq++
	r.Local = p.localSeq
	p.seq++
	r.Seq = p.seq
	p.trace = append(p.trace, r)
	p.mu.Unlock()
}

// appendRemoteTrace 把远端回传的 trace 片段按序并入本地 trace（重排 seq）。
func (p *Program) appendRemoteTrace(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var recs []TraceRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		p.warnf("trace 聚合失败: %v", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range recs {
		p.seq++
		recs[i].Seq = p.seq
		p.trace = append(p.trace, recs[i])
	}
}

func (p *Program) newTraceID() string {
	n := atomic.AddInt64(&p.reqSeq, 1)
	return fmt.Sprintf("%s-t%d", p.nodeTag, n)
}''')

# 5) evalMatch 改用 interp.addTrace
s = s.replace('in.p.addTrace(rec)', 'in.addTrace(rec)')

# 6) interp 增加 traceID / depth
s = s.replace('''type interp struct {
	p        *Program
	global   *env
	noRemote bool // 节点内部执行时禁止再次远端转发，避免回路
}''', '''type interp struct {
	p        *Program
	global   *env
	noRemote bool   // 节点内部执行时禁止再次远端转发，避免回路
	traceID  string // 全局 trace 串联键
	depth    int    // 跨节点调用深度
}''')

# 7) Run / RunHandler 生成 traceID
s = s.replace('''	in := &interp{p: p, global: newEnv(nil)}
	ret := in.callFn(fd, args)
	return p.snapshot(ret), nil''', '''	in := &interp{p: p, global: newEnv(nil), traceID: p.newTraceID()}
	ret := in.callFn(fd, args)
	return p.snapshot(ret, in.traceID), nil''')

s = s.replace('''	in := &interp{p: p, global: newEnv(nil)}
	e := newEnv(nil)
	for i, port := range nd.Ports {''', '''	in := &interp{p: p, global: newEnv(nil), traceID: p.newTraceID()}
	e := newEnv(nil)
	for i, port := range nd.Ports {''')

s = s.replace('''	return p.snapshot(Unit()), nil''', '''	return p.snapshot(Unit(), in.traceID), nil''')

s = s.replace('''func (p *Program) snapshot(ret Value) Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Result{
		Returned: ret,''', '''func (p *Program) snapshot(ret Value, traceID string) Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Result{
		TraceID:  traceID,
		Returned: ret,''')

# 8) EnableCluster 记录自身地址
s = s.replace('''func (p *Program) EnableCluster(self string, timeout time.Duration, logf func(string, ...any)) {
	p.Cluster = &cluster.Dispatcher{Self: self, Timeout: timeout, Table: p.Table, Log: logf}
}''', '''func (p *Program) EnableCluster(self string, timeout time.Duration, logf func(string, ...any)) {
	p.selfAddr = self
	p.Cluster = &cluster.Dispatcher{Self: self, Timeout: timeout, Table: p.Table, Log: logf}
}''')

# 9) callMethod 远端调用带上 trace，并聚合返回片段
s = s.replace('''			id := in.p.nextReqID(inst.Name, name)
			resp, node, derr := in.p.Cluster.Dispatch(inst.Name, name, wire, id)''', '''			id := in.p.nextReqID(inst.Name, name)
			resp, node, derr := in.p.Cluster.Dispatch(inst.Name, name, wire, id, in.traceID, in.depth+1)''')

s = s.replace('''				if resp.Deduped {
					in.p.addOutput("rpc deduped " + id)
				}
				return Unit()''', '''				in.p.appendRemoteTrace(resp.Trace)
				if resp.Deduped {
					in.p.addOutput("rpc deduped " + id)
				}
				return Unit()''')

# 10) CallCapability 新签名：接收 traceID/depth，回传 trace 片段
old = '''func (p *Program) CallCapability(instance, method string, args []cluster.Value) (outs, learns []string, err error) {
	inst, ok := p.DB.Lookup(instance)
	if !ok {
		return nil, nil, fmt.Errorf("instance %q 不存在", instance)
	}
	vals := make([]Value, len(args))
	for i, a := range args {
		v, verr := p.fromWire(a)
		if verr != nil {
			return nil, nil, verr
		}
		vals[i] = v
	}

	p.mu.Lock()
	outMark, learnMark := len(p.outputs), len(p.learns)
	p.mu.Unlock()

	in := &interp{p: p, global: newEnv(nil), noRemote: true}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(runtimeError); ok {
					err = re
					return
				}
				panic(r)
			}
		}()
		in.callMethodLocal(inst, method, vals)
	}()

	p.mu.Lock()
	outs = append([]string(nil), p.outputs[outMark:]...)
	learns = append([]string(nil), p.learns[learnMark:]...)
	p.mu.Unlock()
	return outs, learns, err
}'''

new = '''func (p *Program) CallCapability(instance, method string, args []cluster.Value, traceID string, depth int) (res cluster.ExecResult, err error) {
	inst, ok := p.DB.Lookup(instance)
	if !ok {
		return res, fmt.Errorf("instance %q 不存在", instance)
	}
	vals := make([]Value, len(args))
	for i, a := range args {
		v, verr := p.fromWire(a)
		if verr != nil {
			return res, verr
		}
		vals[i] = v
	}

	p.mu.Lock()
	outMark, learnMark, traceMark := len(p.outputs), len(p.learns), len(p.trace)
	p.mu.Unlock()

	if traceID == "" {
		traceID = p.newTraceID() // 调用方没带 trace 时自己起一条
	}
	in := &interp{p: p, global: newEnv(nil), noRemote: true, traceID: traceID, depth: depth}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if re, ok := r.(runtimeError); ok {
					err = re
					return
				}
				panic(r)
			}
		}()
		in.callMethodLocal(inst, method, vals)
	}()

	p.mu.Lock()
	res.Outputs = append([]string(nil), p.outputs[outMark:]...)
	res.Learns = append([]string(nil), p.learns[learnMark:]...)
	recs := append([]TraceRecord(nil), p.trace[traceMark:]...)
	p.mu.Unlock()

	if len(recs) > 0 {
		raw, merr := json.Marshal(recs)
		if merr != nil {
			return res, merr
		}
		res.Trace = raw
	}
	return res, err
}'''

assert old in s
s = s.replace(old, new)

open(p, 'w').write(s)
print('interp patched for trace')
