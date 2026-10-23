p = 'internal/runtime/logidb.go'
s = open(p).read()
s = s.replace('''type TraceRecord struct {
	Seq        int         `json:"seq"`''', '''type TraceRecord struct {
	TraceID    string      `json:"trace_id,omitempty"`
	Node       string      `json:"node,omitempty"`
	Depth      int         `json:"depth,omitempty"`
	Local      int         `json:"local,omitempty"`
	Seq        int         `json:"seq"`''')
open(p, 'w').write(s)

p = 'internal/runtime/interp.go'
s = open(p).read()
s = s.replace('''		in.p.addTrace(TraceRecord{
			Subject: subj.String(),''', '''		in.addTrace(TraceRecord{
			Subject: subj.String(),''')
open(p, 'w').write(s)
print('patched')
