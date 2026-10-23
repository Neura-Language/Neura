package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"neura/internal/astdump"
	"neura/internal/checker"
	"neura/internal/cluster"
	"neura/internal/diag"
	"neura/internal/lexer"
	"neura/internal/parser"
	"neura/internal/runtime"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "run":
			runCmd(args[1:])
			return
		case "serve":
			serveCmd(args[1:])
			return
		case "rpc":
			rpcCmd(args[1:])
			return
		}
	}
	compileCmd(args)
}

// ---------------- 编译期命令 ----------------

func compileCmd(args []string) {
	fs := flag.NewFlagSet("neura", flag.ExitOnError)
	dumpTokens := fs.Bool("dump-tokens", false, "dump token stream and exit")
	dumpAST := fs.Bool("dump-ast", false, "parse and dump AST")
	dumpErrors := fs.Bool("dump-errors", false, "parse/check and dump diagnostics as `line:col CODE msg`")
	check := fs.Bool("check", false, "run parser + checker (Phase 3)")
	emitExpected := fs.Bool("emit-expected", false, "输出 JSON：把 parser/checker 的实际 span 作为期望值（§17 阶段 B）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: neura --dump-tokens|--dump-ast|--dump-errors|--check <file.neura>\n")
		fmt.Fprintf(os.Stderr, "       neura run <file.neura> [--entry fn] [--handler neuron] [--arg v]... [--trace path] [--node addr] [--root-key K]\n")
		fmt.Fprintf(os.Stderr, "       neura serve <file.neura> --addr host:port [--root-key K]\n")
		fmt.Fprintf(os.Stderr, "       neura rpc --addr host:port --instance I --method m [--id key] [--arg v]... [--root-key K]\n")
		fs.PrintDefaults()
	}
	fs.Parse(reorder(args, map[string]bool{
		"dump-tokens": true, "dump-ast": true, "dump-errors": true, "check": true,
		"emit-expected": true,
	}))

	if *emitExpected {
		if fs.NArg() == 0 {
			fs.Usage()
			os.Exit(2)
		}
		writeExpected(fs.Args())
		return
	}

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	src := readFile(path)

	if *dumpTokens {
		toks, errs := lexer.Lex(src)
		fmt.Printf("// %s: %d tokens, %d errors\n", path, len(toks), len(errs))
		for i, t := range toks {
			fmt.Printf("%4d  %4d:%-4d  %-14s %s%s\n", i, t.Line, t.Col, t.Kind, render(t), marker(t))
		}
		if len(errs) > 0 {
			for _, e := range errs {
				fmt.Fprintf(os.Stderr, "%s:%d:%d: %s\n", path, e.Line, e.Col, e.Msg)
			}
			os.Exit(1)
		}
		return
	}

	file, errs := parser.Parse(src)
	if *dumpAST {
		fmt.Print(astdump.String(file))
	}
	var logs []string
	if len(errs) == 0 && (*check || *dumpErrors) {
		res := checker.Check(file)
		logs = res.Logs
		errs = append(errs, res.Errors...)
	}
	for _, l := range logs {
		fmt.Fprintf(os.Stderr, "%s: %s\n", path, l)
	}
	report(path, errs)

	if !*dumpAST && !*dumpErrors && !*check {
		fs.Usage()
		os.Exit(2)
	}
}

func report(path string, errs []diag.Error) {
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "%s:%d:%d: %s: %s\n", path, e.Pos.Line, e.Pos.Col, e.Code, e.Msg)
	}
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "// %d diagnostic(s)\n", len(errs))
		os.Exit(1)
	}
}

// ---------------- run（单机 / 跨节点客户端） ----------------

type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	entry := fs.String("entry", "main", "入口函数名")
	handler := fs.String("handler", "", "执行 neuron 的 on signal（<neuron> 或 <neuron>.<signal>）")
	tracePath := fs.String("trace", "", "MatchTrace JSONL 输出路径")
	nodeAddr := fs.String("node", "", "本节点地址（命中放置列表时本地执行；空表示全部走远端）")
	timeout := fs.Duration("timeout", 3*time.Second, "单节点 RPC 超时")
	rootKey := fs.String("root-key", "", "S2S Root Key（64 hex 或 32 字节 base64）；设置后跨节点流量加密")
	noCheck := fs.Bool("no-check", false, "跳过静态检查（不推荐）")
	var argList strList
	fs.Var(&argList, "arg", "实参：整数 / 小数 / @实例名 / 其他按字符串（可重复，按顺序绑定）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: neura run <file.neura> [--entry fn] [--handler neuron[.signal]] [--arg v]... [--trace path] [--node addr] [--root-key K]\n")
		fs.PrintDefaults()
	}
	fs.Parse(reorder(args, map[string]bool{"no-check": true}))

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	prog := loadProgram(path, *noCheck)

	var rk *cluster.RootKey
	if *rootKey != "" {
		rk = mustParseRootKey("run", *rootKey)
	}

	if len(prog.Table.Instances()) > 0 {
		logf := func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "[cluster] "+format+"\n", a...)
		}
		if rk != nil {
			prog.EnableClusterS2S(*nodeAddr, *timeout, rk, logf)
			fmt.Fprintf(os.Stderr, "[cluster] s2s=on root_fp=%s\n", rk.Fingerprint())
		} else {
			prog.EnableCluster(*nodeAddr, *timeout, logf)
			fmt.Fprintf(os.Stderr, "[cluster] s2s=off（未设置 --root-key，跨节点明文传输）\n")
		}
		fmt.Fprintf(os.Stderr, "[cluster] self=%q placements=%v\n", *nodeAddr, prog.Placements())
	}

	values := make([]runtime.Value, 0, len(argList))
	for _, a := range argList {
		values = append(values, parseArg(prog, a))
	}

	var res runtime.Result
	var err error
	if *handler != "" {
		res, err = prog.RunHandler(*handler, values)
	} else {
		res, err = prog.Run(*entry, values)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}

	printRun(res)

	if *tracePath != "" {
		if len(res.Trace) == 0 {
			fmt.Fprintf(os.Stderr, "trace: 0 records（该入口没有执行 match ... against LogicDB，未生成文件）\n")
			return
		}
		f, err := os.Create(*tracePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trace: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		if err := prog.WriteTrace(f); err != nil {
			fmt.Fprintf(os.Stderr, "trace: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("trace: %s (%d records)\n", *tracePath, len(res.Trace))
	}
}

// ---------------- serve（跨节点服务端） ----------------

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7001", "监听地址 host:port")
	rootKey := fs.String("root-key", "", "S2S Root Key（64 hex 或 32 字节 base64）；设置后节点要求加密握手")
	noCheck := fs.Bool("no-check", false, "跳过静态检查（不推荐）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: neura serve <file.neura> [--addr host:port] [--root-key K]\n")
		fs.PrintDefaults()
	}
	fs.Parse(reorder(args, map[string]bool{"no-check": true}))

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	prog, err := tryLoad(path, *noCheck)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}

	var rk *cluster.RootKey
	if *rootKey != "" {
		rk = mustParseRootKey("serve", *rootKey)
	}

	// 可热替换执行器：SIGHUP 时整体换掉 Program，监听 socket 与在途请求不受影响。
	sw := cluster.NewSwappable(prog)
	node := cluster.NewNode(*addr, sw, func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[node] "+format+"\n", a...)
	})
	if rk != nil {
		node.EnableS2S(rk)
	}
	if err := node.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
	prog.SetNodeAddr(node.Addr())
	fmt.Printf("neura node listening on %s (pid=%d)\n", node.Addr(), os.Getpid())
	fmt.Printf("instances: %d %v\n", prog.DB.Len(), instanceNames(prog))
	fmt.Printf("placements: %v\n", prog.Placements())
	if rk != nil {
		fmt.Printf("s2s: on (root_fp=%s, HKDF-SHA256 + AES-256-GCM, replay window=%s)\n",
			rk.Fingerprint(), cluster.MaxClockSkew)
	} else {
		fmt.Println("s2s: off（未设置 --root-key，明文 JSON-over-TCP）")
	}
	fmt.Println("waiting for JSON-over-TCP requests ... (Ctrl-C 停止, kill -HUP 热加载)")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	go func() {
		for range sigCh {
			np, err := tryLoad(path, *noCheck)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[reload] 失败，继续使用旧版本: %v\n", err)
				continue
			}
			np.SetNodeAddr(node.Addr())
			sw.Swap(np) // 原子替换：在途请求用旧 Program，新请求用新 Program
			fmt.Printf("[reload] ok: instances=%d %v placements=%v\n",
				np.DB.Len(), instanceNames(np), np.Placements())
		}
	}()

	node.Serve()
}

func instanceNames(prog *runtime.Program) []string {
	out := []string{}
	for _, name := range prog.Table.Instances() {
		out = append(out, name)
	}
	return out
}

// ---------------- rpc（手工发一次请求，验证幂等/failover） ----------------

func rpcCmd(args []string) {
	fs := flag.NewFlagSet("rpc", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7001", "目标节点 host:port")
	instance := fs.String("instance", "", "实例名")
	method := fs.String("method", "run", "capability 方法名")
	id := fs.String("id", "", "幂等键（同一 key 只执行一次）")
	timeout := fs.Duration("timeout", 3*time.Second, "超时")
	rootKey := fs.String("root-key", "", "S2S Root Key（64 hex 或 32 字节 base64）；对端开启 S2S 时必填")
	var argList strList
	fs.Var(&argList, "arg", "实参：整数 / 小数 / 其他按字符串（可重复）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: neura rpc --addr host:port --instance I [--method run] [--id key] [--arg v]... [--root-key K]\n")
		fs.PrintDefaults()
	}
	fs.Parse(reorder(args, map[string]bool{}))

	if *instance == "" {
		fs.Usage()
		os.Exit(2)
	}

	req := cluster.Request{ID: *id, Instance: *instance, Method: *method}
	for _, a := range argList {
		req.Args = append(req.Args, wireArg(a))
	}

	var (
		resp cluster.Response
		err  error
	)
	if *rootKey != "" {
		resp, err = cluster.CallSecure(*addr, mustParseRootKey("rpc", *rootKey), req, *timeout)
	} else {
		resp, err = cluster.Call(*addr, req, *timeout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc: %v\n", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Println(string(b))
	if !resp.OK {
		os.Exit(1)
	}
}

// mustParseRootKey 解析 --root-key；格式错误直接退出，避免静默降级成明文。
func mustParseRootKey(cmd, s string) *cluster.RootKey {
	rk, err := cluster.ParseRootKey(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(2)
	}
	return rk
}

func wireArg(s string) cluster.Value {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return cluster.Value{Kind: "int", I: i}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return cluster.Value{Kind: "float", F: f}
	}
	return cluster.Value{Kind: "string", S: s}
}

// ---------------- 公共 ----------------

func loadProgram(path string, noCheck bool) *runtime.Program {
	prog, err := tryLoad(path, noCheck)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
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
			fmt.Fprintf(os.Stderr, "%s: %s\n", path, l)
		}
		if len(cres.Errors) > 0 {
			return nil, fmt.Errorf("%s: %d 个静态错误（首个：%d:%d %s %s）",
				path, len(cres.Errors), cres.Errors[0].Pos.Line, cres.Errors[0].Pos.Col,
				cres.Errors[0].Code, cres.Errors[0].Msg)
		}
	}
	return runtime.NewProgram(file), nil
}

func parseArg(prog *runtime.Program, s string) runtime.Value {
	if strings.HasPrefix(s, "@") {
		if inst, ok := prog.DB.Lookup(s[1:]); ok {
			return runtime.InstV(inst)
		}
		fmt.Fprintf(os.Stderr, "run: 实例 %q 不存在，按单元值处理\n", s[1:])
		return runtime.Unit()
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return runtime.IntV(i)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return runtime.FloatV(f)
	}
	return runtime.StringV(s)
}

func printRun(res runtime.Result) {
	fmt.Println("=== output ===")
	for _, o := range res.Outputs {
		fmt.Println(o)
	}
	if len(res.Learns) > 0 {
		fmt.Println("=== learn ===")
		for _, l := range res.Learns {
			fmt.Println(l)
		}
	}
	if len(res.Warnings) > 0 {
		fmt.Println("=== warnings ===")
		for _, w := range res.Warnings {
			fmt.Println(w)
		}
	}
	if len(res.Trace) > 0 {
		fmt.Println("=== match trace ===")
		if res.TraceID != "" {
			fmt.Printf("trace_id: %s\n", res.TraceID)
		}
		for _, r := range res.Trace {
			b, _ := json.Marshal(r)
			fmt.Println(string(b))
		}
	}
	fmt.Println("=== return ===")
	fmt.Println(res.Returned.String())
}

// reorder 把 flag 移到位置参数之前，允许 `neura run file.neura --entry f` 这种写法。
func reorder(args []string, boolFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") || boolFlags[name] {
				continue
			}
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		os.Exit(2)
	}
	return string(data)
}

// expectedDiag / expectedFile 是 §17 阶段 B 的回填格式（由 --emit-expected 生成）。
type expectedDiag struct {
	Line int    `json:"line"`
	Col  int    `json:"col"`
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

type expectedFile struct {
	File        string         `json:"file"`
	Diagnostics []expectedDiag `json:"diagnostics"`
}

// writeExpected 对每个文件跑 parser（必要时 checker），把实际诊断原样输出为 JSON。
func writeExpected(paths []string) {
	out := make([]expectedFile, 0, len(paths))
	for _, path := range paths {
		src := readFile(path)
		file, errs := parser.Parse(src)
		if len(errs) == 0 {
			errs = checker.Check(file).Errors
		}
		ef := expectedFile{File: path, Diagnostics: []expectedDiag{}}
		for _, e := range errs {
			ef.Diagnostics = append(ef.Diagnostics, expectedDiag{
				Line: e.Pos.Line,
				Col:  e.Pos.Col,
				Code: e.Code,
				Msg:  e.Msg,
			})
		}
		out = append(out, ef)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "emit-expected: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func render(t lexer.Token) string {
	switch t.Kind {
	case lexer.EOF:
		return "<EOF>"
	case lexer.VirtualSemi:
		return ";ᵥ"
	}
	return fmt.Sprintf("%q", t.Lexeme)
}

func marker(t lexer.Token) string {
	switch {
	case t.Kind == lexer.VirtualSemi:
		return "  #inserted"
	case t.IsBuiltinType():
		return "  #builtin-type"
	}
	return ""
}
