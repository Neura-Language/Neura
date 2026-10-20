// Package astdump 把 AST 打印成缩进文本（--dump-ast 用）。
// 用反射遍历，避免每加一个节点就要改一次打印器；字段顺序即结构体字段顺序。
package astdump

import (
	"fmt"
	"reflect"
	"strings"

	"neura/internal/diag"
)

var posType = reflect.TypeOf(diag.Pos{})

// String 返回 f 的缩进文本表示。
func String(v any) string {
	var b strings.Builder
	write(&b, reflect.ValueOf(v), 0, "")
	return b.String()
}

func write(b *strings.Builder, v reflect.Value, indent int, label string) {
	pad := strings.Repeat("  ", indent)

	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr) {
		if v.IsNil() {
			b.WriteString(pad)
			if label != "" {
				fmt.Fprintf(b, "%s: ", label)
			}
			b.WriteString("nil\n")
			return
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == posType {
			p := v.Interface().(diag.Pos)
			b.WriteString(pad)
			if label != "" {
				fmt.Fprintf(b, "%s: ", label)
			}
			fmt.Fprintf(b, "%s\n", p)
			return
		}
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		b.WriteString(v.Type().Name())
		b.WriteString("\n")
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // 未导出
				continue
			}
			write(b, v.Field(i), indent+1, f.Name)
		}

	case reflect.Slice:
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		if v.Len() == 0 {
			b.WriteString("[]\n")
			return
		}
		fmt.Fprintf(b, "[%d]\n", v.Len())
		for i := 0; i < v.Len(); i++ {
			write(b, v.Index(i), indent+1, fmt.Sprintf("[%d]", i))
		}

	case reflect.String:
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		fmt.Fprintf(b, "%q\n", v.String())

	case reflect.Bool:
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		fmt.Fprintf(b, "%t\n", v.Bool())

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		fmt.Fprintf(b, "%d\n", v.Int())

	default:
		b.WriteString(pad)
		if label != "" {
			fmt.Fprintf(b, "%s: ", label)
		}
		fmt.Fprintf(b, "%v\n", v.Interface())
	}
}
