package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind 是运行时值类别。
type Kind int

const (
	KUnit Kind = iota
	KInt
	KFloat
	KBool
	KString
	KSize
	KDuration
	KInstance
)

// Value 是运行时值。Size 存字节数（F）与原始文本（S）；Duration 存纳秒（F）与原始文本。
type Value struct {
	Kind Kind
	I    int64
	F    float64
	B    bool
	S    string
	Inst *Instance
}

func Unit() Value            { return Value{Kind: KUnit} }
func IntV(i int64) Value     { return Value{Kind: KInt, I: i} }
func FloatV(f float64) Value { return Value{Kind: KFloat, F: f} }
func BoolV(b bool) Value     { return Value{Kind: KBool, B: b} }
func StringV(s string) Value { return Value{Kind: KString, S: s} }
func InstV(i *Instance) Value {
	return Value{Kind: KInstance, Inst: i}
}

func (v Value) String() string {
	switch v.Kind {
	case KUnit:
		return "unit"
	case KInt:
		return strconv.FormatInt(v.I, 10)
	case KFloat:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case KBool:
		if v.B {
			return "true"
		}
		return "false"
	case KString:
		return v.S
	case KSize, KDuration:
		return v.S
	case KInstance:
		if v.Inst == nil {
			return "instance(nil)"
		}
		return v.Inst.Name
	}
	return "?"
}

// Canon 是与比较/哈希/相似度用的规范化文本（类型敏感）。
func (v Value) Canon() string {
	if v.Kind == KInstance {
		if v.Inst == nil {
			return "instance:nil"
		}
		return "instance:" + v.Inst.Name
	}
	return fmt.Sprintf("%d:%s", v.Kind, v.String())
}

func (v Value) IsNumeric() bool {
	switch v.Kind {
	case KInt, KFloat, KSize, KDuration:
		return true
	}
	return false
}

func (v Value) AsFloat() float64 {
	switch v.Kind {
	case KInt:
		return float64(v.I)
	case KFloat, KSize, KDuration:
		return v.F
	}
	return 0
}

// parseNumberUnit 把 "1.5GB" / "20ms" 拆成数值与单位。
func parseNumberUnit(s string) (float64, string, bool) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, "", false
	}
	return num, s[i:], true
}

var sizeMultiplier = map[string]float64{
	"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
	"KiB": 1024, "MiB": 1024 * 1024, "GiB": 1024 * 1024 * 1024, "TiB": 1024 * 1024 * 1024 * 1024,
}

var durationMultiplier = map[string]float64{
	"ns": 1, "us": 1e3, "µs": 1e3, "ms": 1e6,
	"s": 1e9, "m": 60e9, "h": 3600e9, "d": 86400e9,
}

func sizeValue(text string) Value {
	n, unit, ok := parseNumberUnit(text)
	if !ok {
		return Value{Kind: KSize, S: text}
	}
	mult := sizeMultiplier[unit]
	return Value{Kind: KSize, F: n * mult, S: text}
}

func durationValue(text string) Value {
	n, unit, ok := parseNumberUnit(text)
	if !ok {
		return Value{Kind: KDuration, S: text}
	}
	mult := durationMultiplier[unit]
	return Value{Kind: KDuration, F: n * mult, S: text}
}

func unquoteString(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		body := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) {
				i++
				switch body[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(body[i])
				}
				continue
			}
			b.WriteByte(body[i])
		}
		return b.String()
	}
	return s
}
