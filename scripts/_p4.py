p = 'internal/runtime/runtime_test.go'
s = open(p).read()

s = s.replace('''		var buf bytes.Buffer
		if err := p.WriteTrace(&buf); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = buf.String()
		} else if first != buf.String() {
			t.Fatalf("trace not deterministic:\\n%s\\nvs\\n%s", first, buf.String())
		}''', '''		var buf bytes.Buffer
		if err := p.WriteTrace(&buf); err != nil {
			t.Fatal(err)
		}
		// trace_id 每次运行必然唯一（用于跨节点串联），确定性只比对其余字段
		got := normalizeTraceIDs(t, buf.String())
		if first == "" {
			first = got
		} else if first != got {
			t.Fatalf("trace not deterministic:\\n%s\\nvs\\n%s", first, got)
		}''')

s = s.replace('''func mustRun(t *testing.T, p *Program, entry string, args []Value) Result {''', '''// normalizeTraceIDs 去掉 trace_id 字段，便于比较「同一输入的可复现性」。
func normalizeTraceIDs(t *testing.T, jsonl string) string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\\n") {
		if line == "" {
			continue
		}
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("bad trace line: %v (%s)", err, line)
		}
		rec.TraceID = ""
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return strings.Join(out, "\\n")
}

func mustRun(t *testing.T, p *Program, entry string, args []Value) Result {''')

open(p, 'w').write(s)
print('patched determinism test')
