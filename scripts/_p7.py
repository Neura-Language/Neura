p = 'internal/runtime/interp.go'
s = open(p).read()
s = s.replace('''// EnableCluster 启用跨节点调度（self 为空表示不认领任何本地放置）。''', '''// SetNodeAddr 设置本节点在 trace 里的标识（serve 时用实际监听地址）。
func (p *Program) SetNodeAddr(addr string) {
	p.selfAddr = addr
}

// EnableCluster 启用跨节点调度（self 为空表示不认领任何本地放置）。''')

# trace 记录里 node 为空时回落到 nodeTag，避免跨节点记录看不出是谁
s = s.replace('''	r.Node = in.p.selfAddr''', '''	r.Node = in.p.selfAddr
	if r.Node == "" {
		r.Node = in.p.nodeTag
	}''')
open(p, 'w').write(s)

p = 'cmd/neura/main.go'
s = open(p).read()
s = s.replace('''	fmt.Printf("neura node listening on %s (pid=%d)\\n", node.Addr(), os.Getpid())''',
'''	prog.SetNodeAddr(node.Addr())
	fmt.Printf("neura node listening on %s (pid=%d)\\n", node.Addr(), os.Getpid())''')
s = s.replace('''			sw.Swap(np) // 原子替换：在途请求用旧 Program，新请求用新 Program''',
'''			np.SetNodeAddr(node.Addr())
			sw.Swap(np) // 原子替换：在途请求用旧 Program，新请求用新 Program''')
open(p, 'w').write(s)

p = 'internal/runtime/net_test.go'
s = open(p).read()
s = s.replace('''	go n.Serve()
	t.Cleanup(func() { _ = n.Close() })
	return n''', '''	go n.Serve()
	if prog != nil {
		prog.SetNodeAddr(n.Addr())
	}
	t.Cleanup(func() { _ = n.Close() })
	return n''')
open(p, 'w').write(s)
print('patched node addr')
