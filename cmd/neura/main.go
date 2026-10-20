package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"neura/internal/astdump"
	"neura/internal/checker"
	"neura/internal/diag"
	"neura/internal/lexer"
	"neura/internal/parser"
	"neura/internal/runtime"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "run" {
		runCmd(args[1:])
		return
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
		fmt.Fprintf(os.Stderr, "       neura run <file.neura> [--entry fn] [--handler neuron] [--arg v]... [--trace path]\n")
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

// ---------------- run 子命令（Phase 4） ----------------

type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	entry := fs.String("entry", "main", "入口函数名")
	handler := fs.String("handler", "", "直接执行某个 neuron 的 handle 块（emit 可达）")
	tracePath := fs.String("trace", "", "MatchTrace JSONL 输出路径")
	noCheck := fs.Bool("no-check", false, "跳过静态检查（不推荐）")
	var argList strList
	fs.Var(&argList, "arg", "实参：整数 / @实例名 / 其他按字符串（可重复，按顺序绑定）")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: neura run <file.neura> [--entry fn] [--handler neuron] [--arg v]... [--trace path]\n")
		fs.PrintDefaults()
	}
	fs.Parse(reorder(args, map[string]bool{"no-check": true}))

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	src := readFile(path)

	file, perrs := parser.Parse(src)
	if len(perrs) > 0 {
		report(path, perrs)
		return
	}
	if !*noCheck {
		cres := checker.Check(file)
		for _, l := range cres.Logs {
			fmt.Fprintf(os.Stderr, "%s: %s\n", path, l)
		}
		if len(cres.Errors) > 0 {
			report(path, cres.Errors)
			return
		}
	}

	prog := runtime.NewProgram(file)
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
		for _, r := range res.Trace {
			b, _ := json.Marshal(r)
			fmt.Println(string(b))
		}
	}
	fmt.Println("=== return ===")
	fmt.Println(res.Returned.String())
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
// 这是阶段 B 的「parser 输出替换人工数」：JSON 进版本库，CI 阻断式比对。
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
