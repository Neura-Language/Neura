package checker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"neura/internal/parser"
)

// expectedFileJSON 与 `neura --emit-expected` 的输出格式一致。
type expectedFileJSON struct {
	File        string `json:"file"`
	Diagnostics []struct {
		Line int    `json:"line"`
		Col  int    `json:"col"`
		Code string `json:"code"`
		Msg  string `json:"msg"`
	} `json:"diagnostics"`
}

// TestExpectedSpansBackfill 是 §17 阶段 B 的阻断式对拍：
// 期望值来自 testdata/expected_errors.json（由 --emit-expected 生成，非人工数）。
// 任何 span / 错误码漂移都会让这个测试失败，直到有人重新生成并 review 过 diff。
func TestExpectedSpansBackfill(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "expected_errors.json"))
	if err != nil {
		t.Fatalf("read expected_errors.json: %v（先跑 scripts/gen_expected.sh）", err)
	}
	var files []expectedFileJSON
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("parse expected_errors.json: %v", err)
	}
	if len(files) != 12 {
		t.Fatalf("expected 12 §17 negative files, got %d", len(files))
	}

	for _, ef := range files {
		ef := ef
		t.Run(filepath.Base(ef.File), func(t *testing.T) {
			src := readFile(t, filepath.Join("..", "..", ef.File))
			f, perrs := parser.Parse(src)
			errs := perrs
			if len(errs) == 0 {
				errs = Check(f).Errors
			}

			if len(errs) != len(ef.Diagnostics) {
				t.Fatalf("diagnostics = %d, want %d\n got=%v", len(errs), len(ef.Diagnostics), errs)
			}
			for i, want := range ef.Diagnostics {
				got := errs[i]
				if got.Pos.Line != want.Line || got.Pos.Col != want.Col || got.Code != want.Code {
					t.Fatalf("[%d] got %d:%d %s, want %d:%d %s\n  msg=%s",
						i, got.Pos.Line, got.Pos.Col, got.Code,
						want.Line, want.Col, want.Code, got.Msg)
				}
				if got.Msg != want.Msg {
					t.Fatalf("[%d] message drifted:\n got=%q\nwant=%q", i, got.Msg, want.Msg)
				}
			}
		})
	}
}
