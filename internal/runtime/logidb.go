package runtime

import (
	"sort"
	"strings"
)

// Instance 是一条已部署的 logic 实例（structure 的字段值）。
type Instance struct {
	Name   string
	Logic  string
	Fields map[string]Value
}

// Field 取字段值。
func (i *Instance) Field(name string) (Value, bool) {
	v, ok := i.Fields[name]
	return v, ok
}

// LogicDB 是内存版实例库：按 logic 的倒排索引 + 按名字索引 + 字段倒排索引。
// 所有遍历出口都排序，保证确定性（无 map 迭代顺序泄漏）。
type LogicDB struct {
	byName  map[string]*Instance
	byLogic map[string][]*Instance
	byField map[string]map[string][]*Instance // field → canon(value) → instances
}

func NewLogicDB() *LogicDB {
	return &LogicDB{
		byName:  map[string]*Instance{},
		byLogic: map[string][]*Instance{},
		byField: map[string]map[string][]*Instance{},
	}
}

// Add 注册实例；同名覆盖（保持接口幂等）。
func (db *LogicDB) Add(in *Instance) {
	if in.Fields == nil {
		in.Fields = map[string]Value{}
	}
	db.byName[in.Name] = in

	list := db.byLogic[in.Logic]
	replaced := false
	for i, old := range list {
		if old.Name == in.Name {
			list[i] = in
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, in)
	}
	db.byLogic[in.Logic] = list

	for f, v := range in.Fields {
		if db.byField[f] == nil {
			db.byField[f] = map[string][]*Instance{}
		}
		k := v.Canon()
		db.byField[f][k] = append(db.byField[f][k], in)
	}
}

// Lookup 按实例名查。
func (db *LogicDB) Lookup(name string) (*Instance, bool) {
	in, ok := db.byName[name]
	return in, ok
}

// InstancesOf 返回某 logic 的全部实例，按名字升序（确定性）。
func (db *LogicDB) InstancesOf(logic string) []*Instance {
	list := append([]*Instance(nil), db.byLogic[logic]...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// FieldIndex 用字段倒排索引查（字段名 + 值）。
func (db *LogicDB) FieldIndex(field string, v Value) []*Instance {
	m := db.byField[field]
	if m == nil {
		return nil
	}
	list := append([]*Instance(nil), m[v.Canon()]...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Len 是实例总数。
func (db *LogicDB) Len() int { return len(db.byName) }

// LogicNames 返回出现过的 logic 名（升序）。
func (db *LogicDB) LogicNames() []string {
	out := make([]string, 0, len(db.byLogic))
	for k := range db.byLogic {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// StrictEqual：字段集合相同且每个字段规范值相同。这是 match 的第一优先级。
func StrictEqual(a, b map[string]Value) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || av.Canon() != bv.Canon() {
			return false
		}
	}
	return true
}

// Similarity：纯函数打分，= 相同字段值个数 / 字段并集大小，值域 [0,1]。
// 不依赖 map 迭代顺序（只做计数），因此可复现。
func Similarity(a, b map[string]Value) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	union := map[string]bool{}
	equal := 0
	for k, av := range a {
		union[k] = true
		if bv, ok := b[k]; ok && av.Canon() == bv.Canon() {
			equal++
		}
	}
	for k := range b {
		union[k] = true
	}
	if len(union) == 0 {
		return 1
	}
	return float64(equal) / float64(len(union))
}

// Candidate 是 match 的一个候选（写入 trace）。
type Candidate struct {
	Instance string  `json:"instance"`
	Logic    string  `json:"logic"`
	Strict   bool    `json:"strict"`
	Score    float64 `json:"score"`
	Guard    bool    `json:"guard"`
}

// TraceRecord 是 MatchTrace 的一行（JSONL）。不含时间戳，保证可复现。
type TraceRecord struct {
	Seq        int         `json:"seq"`
	Subject    string      `json:"subject"`
	Logic      string      `json:"logic"`
	Arm        string      `json:"arm"`
	Candidates []Candidate `json:"candidates"`
	Chosen     string      `json:"chosen"`
	Reason     string      `json:"reason"` // strict / similarity / no-candidate / guard-rejected
	Bind       string      `json:"bind,omitempty"`
}

// sortCandidates：strict 优先 → 相似度降序 → 实例名升序（确定性 tie-break）。
func sortCandidates(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Strict != cs[j].Strict {
			return cs[i].Strict
		}
		if cs[i].Score != cs[j].Score {
			return cs[i].Score > cs[j].Score
		}
		return cs[i].Instance < cs[j].Instance
	})
}

// FormatFields 只用于日志/测试输出（键升序）。
func FormatFields(f map[string]Value) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + f[k].Canon()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
