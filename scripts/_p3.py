p = 'internal/runtime/net_test.go'
s = open(p).read()

s = s.replace('''	outs, learns, err := prog.CallCapability("I", "run", nil)
	if err != nil {
		t.Fatalf("CallCapability: %v", err)
	}
	if len(outs) == 0 || !strings.HasPrefix(outs[0], "invoke I.run") {
		t.Fatalf("outs = %v", outs)
	}
	if len(learns) != 1 {
		t.Fatalf("learns = %v", learns)
	}''', '''	res, err := prog.CallCapability("I", "run", nil, "trace-test", 1)
	if err != nil {
		t.Fatalf("CallCapability: %v", err)
	}
	if len(res.Outputs) == 0 || !strings.HasPrefix(res.Outputs[0], "invoke I.run") {
		t.Fatalf("outs = %v", res.Outputs)
	}
	if len(res.Learns) != 1 {
		t.Fatalf("learns = %v", res.Learns)
	}''')

s = s.replace('''	if _, _, err := prog.CallCapability("Nope", "run", nil); err == nil {
		t.Fatal("未知 instance 应报错")
	}''', '''	if _, err := prog.CallCapability("Nope", "run", nil, "trace-test", 1); err == nil {
		t.Fatal("未知 instance 应报错")
	}''')

s = s.replace('''	if _, _, err := prog2.CallCapability("I", "nope", nil); err != nil {
		t.Logf("unknown method returned error: %v", err)
	}''', '''	if _, err := prog2.CallCapability("I", "nope", nil, "trace-test", 1); err != nil {
		t.Logf("unknown method returned error: %v", err)
	}''')

open(p, 'w').write(s)
print('net_test patched')
