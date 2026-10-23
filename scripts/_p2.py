p = 'internal/cluster/cluster_test.go'
s = open(p).read()

s = s.replace('''func (f *fakeExec) CallCapability(instance, method string, args []Value) ([]string, []string, error) {
	f.mu.Lock()
	f.calls++
	f.last = Request{Instance: instance, Method: method, Args: args}
	delay := f.delay
	err := f.err
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, nil, err
	}
	return []string{"invoke " + instance + "." + method}, []string{"learn 1"}, nil
}''', '''func (f *fakeExec) CallCapability(instance, method string, args []Value, traceID string, depth int) (ExecResult, error) {
	f.mu.Lock()
	f.calls++
	f.last = Request{Instance: instance, Method: method, Args: args, TraceID: traceID, Depth: depth}
	delay := f.delay
	err := f.err
	f.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return ExecResult{}, err
	}
	// 模拟远端产生一条 trace：验证 traceID 透传 + 片段回传
	trace := []byte(`[{"trace_id":"` + traceID + `","node":"fake","subject":"` + instance + `"}]`)
	return ExecResult{
		Outputs: []string{"invoke " + instance + "." + method},
		Learns:  []string{"learn 1"},
		Trace:   trace,
	}, nil
}''')

s = s.replace('d.Dispatch("I", "run", nil, "id-1")', 'd.Dispatch("I", "run", nil, "id-1", "tr-1", 1)')
s = s.replace('d.Dispatch("I", "run", nil, "id-2")', 'd.Dispatch("I", "run", nil, "id-2", "tr-2", 1)')
s = s.replace('d.Dispatch("I", "run", nil, "id-3")', 'd.Dispatch("I", "run", nil, "id-3", "tr-3", 1)')
s = s.replace('d.Dispatch("missing", "run", nil, "id-4")', 'd.Dispatch("missing", "run", nil, "id-4", "tr-4", 1)')

# 新增断言：trace 透传与回传
s = s.replace('''	if exec.count() != 1 {
		t.Fatalf("calls = %d, want 1", exec.count())
	}
}

func TestBadRequestGetsErrorResponse''', '''	if exec.count() != 1 {
		t.Fatalf("calls = %d, want 1", exec.count())
	}
	if string(resp.Trace) == "" {
		t.Fatal("响应应回传远端 trace 片段")
	}
}

func TestBadRequestGetsErrorResponse''')

s = s.replace('''func TestTableOrderDedup(t *testing.T) {''', '''// SIGHUP 热加载用的 Swappable：Swap 后立刻生效，旧执行器不再被使用。
func TestSwappableExecutor(t *testing.T) {
	a := &fakeExec{}
	b := &fakeExec{}
	sw := NewSwappable(a)

	if _, err := sw.CallCapability("I", "run", nil, "tr", 0); err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 || b.count() != 0 {
		t.Fatalf("before swap: a=%d b=%d", a.count(), b.count())
	}

	sw.Swap(b)
	res, err := sw.CallCapability("I", "run", nil, "tr", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("after swap: a=%d b=%d", a.count(), b.count())
	}
	if len(res.Outputs) != 1 || res.Outputs[0] != "invoke I.run" {
		t.Fatalf("outputs = %v", res.Outputs)
	}
}

func TestTableOrderDedup(t *testing.T) {''')

open(p, 'w').write(s)
print('cluster tests patched')
