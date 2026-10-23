p = 'cmd/neura/main.go'
s = open(p).read()

# imports
s = s.replace('''	"os"
	"strconv"''', '''	"os"
	"os/signal"
	"strconv"''')
s = s.replace('''	"strings"
	"time"''', '''	"strings"
	"syscall"
	"time"''')

# serve：Swappable + SIGHUP 热加载
old = '''	path := fs.Arg(0)
	prog := loadProgram(path, *noCheck)

	node := cluster.NewNode(*addr, prog, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[node] "+format+"\\n", a...)
	})
	if err := node.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\\n", err)
		os.Exit(1)
	}
	fmt.Printf("neura node listening on %s\\n", node.Addr())
	fmt.Printf("instances: %d %v\\n", prog.DB.Len(), instanceNames(prog))
	fmt.Printf("placements: %v\\n", prog.Placements())
	fmt.Println("waiting for JSON-over-TCP requests ... (Ctrl-C to stop)")
	node.Serve()'''

new = '''	path := fs.Arg(0)
	prog, err := tryLoad(path, *noCheck)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\\n", err)
		os.Exit(1)
	}

	// 可热替换执行器：SIGHUP 时整体换掉 Program，监听 socket 与在途请求不受影响。
	sw := cluster.NewSwappable(prog)
	node := cluster.NewNode(*addr, sw, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[node] "+format+"\\n", a...)
	})
	if err := node.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\\n", err)
		os.Exit(1)
	}
	fmt.Printf("neura node listening on %s (pid=%d)\\n", node.Addr(), os.Getpid())
	fmt.Printf("instances: %d %v\\n", prog.DB.Len(), instanceNames(prog))
	fmt.Printf("placements: %v\\n", prog.Placements())
	fmt.Println("waiting for JSON-over-TCP requests ... (Ctrl-C 停止, kill -HUP 热加载)")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			np, err := tryLoad(path, *noCheck)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[reload] 失败，继续使用旧版本: %v\\n", err)
				continue
			}
			sw.Swap(np) // 原子替换：在途请求用旧 Program，新请求用新 Program
			fmt.Printf("[reload] ok: instances=%d %v placements=%v\\n",
				np.DB.Len(), instanceNames(np), np.Placements())
		}
	}()

	node.Serve()'''

assert old in s
s = s.replace(old, new)

# 统一加载入口：loadProgram 复用 tryLoad
old_load = '''func loadProgram(path string, noCheck bool) *runtime.Program {
	src := readFile(path)
	file, perrs := parser.Parse(src)
	if len(perrs) > 0 {
		report(path, perrs)
	}
	if !noCheck {
		cres := checker.Check(file)
		for _, l := range cres.Logs {
			fmt.Fprintf(os.Stderr, "%s: %s\\n", path, l)
		}
		if len(cres.Errors) > 0 {
			report(path, cres.Errors)
		}
	}
	return runtime.NewProgram(file)
}'''

new_load = '''func loadProgram(path string, noCheck bool) *runtime.Program {
	prog, err := tryLoad(path, noCheck)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\\n", err)
		os.Exit(1)
	}
	return prog
}

// tryLoad 解析 + 检查 + 装载；热加载失败时调用方保留旧版本，不退出进程。
func tryLoad(path string, noCheck bool) (*runtime.Program, error) {
	src := readFile(path)
	file, perrs := parser.Parse(src)
	if len(perrs) > 0 {
		return nil, fmt.Errorf("%s: %d 个语法错误（首个：%d:%d %s %s）",
			path, len(perrs), perrs[0].Pos.Line, perrs[0].Pos.Col, perrs[0].Code, perrs[0].Msg)
	}
	if !noCheck {
		cres := checker.Check(file)
		for _, l := range cres.Logs {
			fmt.Fprintf(os.Stderr, "%s: %s\\n", path, l)
		}
		if len(cres.Errors) > 0 {
			return nil, fmt.Errorf("%s: %d 个静态错误（首个：%d:%d %s %s）",
				path, len(cres.Errors), cres.Errors[0].Pos.Line, cres.Errors[0].Pos.Col,
				cres.Errors[0].Code, cres.Errors[0].Msg)
		}
	}
	return runtime.NewProgram(file), nil
}'''

assert old_load in s
s = s.replace(old_load, new_load)

# run 里也改用 loadProgram（原本就是）

# printRun 打印 trace_id
s = s.replace('''	if len(res.Trace) > 0 {
		fmt.Println("=== match trace ===")''', '''	if len(res.Trace) > 0 {
		fmt.Println("=== match trace ===")
		if res.TraceID != "" {
			fmt.Printf("trace_id: %s\\n", res.TraceID)
		}''')

open(p, 'w').write(s)
print('main patched')
