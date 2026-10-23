package runtime

import (
	"context"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SimilarityProvider 是 LogicDB 相似度的唯一扩展点。
// Score 必须是纯函数、值域 [0,1]：同一输入必须给出同一分数，
// 否则 MatchTrace 不可复现、跨节点候选排序会漂移。
type SimilarityProvider interface {
	Name() string
	Score(a, b map[string]Value) float64
}

// ---------------- 内置：Jaccard / SimHash ----------------

// JaccardProvider 是 v0.3.7 的默认实现：相同字段值数 / 字段并集。
type JaccardProvider struct{}

func (JaccardProvider) Name() string { return "jaccard" }
func (JaccardProvider) Score(a, b map[string]Value) float64 {
	return Similarity(a, b)
}

// SimHashProvider 对「字段键=规范值」集合做 64 位 SimHash，
// 相似度 = 1 - 汉明距离/64。对字段顺序不敏感，适合高基数文本/标签。
type SimHashProvider struct {
	Bits int // <=0 默认 64
}

func (s SimHashProvider) Name() string { return "simhash" }

func (s SimHashProvider) Score(a, b map[string]Value) float64 {
	bits := s.Bits
	if bits <= 0 || bits > 64 {
		bits = 64
	}
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	ha, hb := simhash(a, bits), simhash(b, bits)
	return 1 - float64(popcount(ha^hb))/float64(bits)
}

func simhash(fields map[string]Value, bits int) uint64 {
	var vector [64]int
	for _, tok := range tokens(fields) {
		h := fnv1a(tok)
		for i := 0; i < bits; i++ {
			if h&(1<<uint(i)) != 0 {
				vector[i]++
			} else {
				vector[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < bits; i++ {
		if vector[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

func tokens(fields map[string]Value) []string {
	out := make([]string, 0, len(fields))
	for k, v := range fields {
		out = append(out, k+"="+v.Canon())
	}
	sort.Strings(out)
	return out
}

func fnv1a(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

func popcount(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}

// ---------------- 外部 Embedding 探针 ----------------

// EmbeddingProbe 是最小外部向量化接口。实现方可以是本地 ONNX、HTTP 服务、
// 或任何第三方 embedding；runtime 不绑定具体 SDK。
type EmbeddingProbe interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// VectorIndex 是外部向量检索探针（HNSW/Milvus/pgvector 等）。
// 预留它是为了未来把「候选召回」下推到向量库，而不是把向量塞进 LogicDB。
type VectorIndex interface {
	Search(ctx context.Context, vec []float32, k int) ([]VectorHit, error)
	Upsert(ctx context.Context, id string, vec []float32) error
}

// VectorHit 是向量检索命中。ID 约定为 instance 名。
type VectorHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// ErrNoVectorIndex 表示未配置外部向量库。
var ErrNoVectorIndex = errors.New("runtime: vector index not configured")

// EmbeddingProvider 是外部 embedding 的适配层。
//
// 强约束：
//   - 向量服务失败/超时必须回退到 Fallback（默认 Jaccard），绝不让调度失败；
//   - Score 必须确定性；外部服务本身做不到确定性时，trace 会标记 provider=embedding，
//     语义上接受「同输入可能不同分数」，但候选 tie-break 仍由实例名保证稳定。
type EmbeddingProvider struct {
	Probe    EmbeddingProbe
	Fallback SimilarityProvider
	Timeout  time.Duration
	Index    VectorIndex
}

func (e *EmbeddingProvider) Name() string {
	if e == nil {
		return "embedding"
	}
	return "embedding"
}

func (e *EmbeddingProvider) fallback() SimilarityProvider {
	if e != nil && e.Fallback != nil {
		return e.Fallback
	}
	return JaccardProvider{}
}

// Score 实现 SimilarityProvider。
func (e *EmbeddingProvider) Score(a, b map[string]Value) float64 {
	if e == nil || e.Probe == nil {
		return e.fallback().Score(a, b)
	}
	va, err := e.embedText(CanonicalText(a))
	if err != nil {
		return e.fallback().Score(a, b)
	}
	vb, err := e.embedText(CanonicalText(b))
	if err != nil {
		return e.fallback().Score(a, b)
	}
	return Cosine(va, vb)
}

// Search 是外部向量召回探针：text → embedding → VectorIndex.Search。
func (e *EmbeddingProvider) Search(ctx context.Context, fields map[string]Value, k int) ([]VectorHit, error) {
	if e == nil || e.Index == nil {
		return nil, ErrNoVectorIndex
	}
	vec, err := e.embed(ctx, CanonicalText(fields))
	if err != nil {
		return nil, err
	}
	if k <= 0 {
		k = 10
	}
	return e.Index.Search(ctx, vec, k)
}

// Upsert 把实例写进外部向量库（供 embedding 探针预热）。
func (e *EmbeddingProvider) Upsert(ctx context.Context, id string, fields map[string]Value) error {
	if e == nil || e.Index == nil {
		return ErrNoVectorIndex
	}
	vec, err := e.embed(ctx, CanonicalText(fields))
	if err != nil {
		return err
	}
	return e.Index.Upsert(ctx, id, vec)
}

func (e *EmbeddingProvider) embedText(text string) ([]float32, error) {
	return e.embed(context.Background(), text)
}

func (e *EmbeddingProvider) embed(ctx context.Context, text string) ([]float32, error) {
	if e.Probe == nil {
		return nil, errors.New("runtime: embedding probe not configured")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 50 * time.Millisecond
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := e.Probe.Embed(cctx, text)
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, errors.New("runtime: empty embedding")
	}
	return v, nil
}

// Cosine 是向量相似度；零向量返回 0。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// CanonicalText 是字段集合的确定性文本，供 embedding / 向量库使用。
// 例：`{amount=7:1.5,region=3:eu}`（键升序，值带 Kind 前缀，避免类型歧义）。
func CanonicalText(fields map[string]Value) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(fields[k].Canon())
	}
	b.WriteByte('}')
	return b.String()
}

// ScoredInstance 是 LogicDB 相似度检索结果。
type ScoredInstance struct {
	Instance string
	Logic    string
	Score    float64
}

// Nearest 是 LogicDB 的纯函数相似度检索：score 降序、实例名升序 tie-break。
// 它不写 trace，供 embedding 召回/离线索引使用；match 主路径仍走 evalMatch。
func (db *LogicDB) Nearest(logic string, subject map[string]Value, provider SimilarityProvider, k int) []ScoredInstance {
	if provider == nil {
		provider = JaccardProvider{}
	}
	out := make([]ScoredInstance, 0)
	for _, in := range db.InstancesOf(logic) {
		out = append(out, ScoredInstance{Instance: in.Name, Logic: in.Logic, Score: provider.Score(subject, in.Fields)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Instance < out[j].Instance
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// FormatScore 只用于日志。
func FormatScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 4, 64)
}
