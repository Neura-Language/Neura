p = 'internal/runtime/runtime_test.go'
s = open(p).read()
s = s.replace('''		rec.TraceID = ""''', '''		rec.TraceID = "" // 每次运行唯一
		rec.Node = ""    // 每个 Program 的节点标识唯一''')
open(p, 'w').write(s)
print('patched normalize')
