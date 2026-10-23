// S2S（server-to-server）加密层：线下 Root Key 握手 + HKDF-SHA256 会话密钥 +
// AES-256-GCM 业务帧 + 3 秒时间戳 / 内存 TTL nonce 表防重放。
//
// 只使用 Go 标准库，没有任何第三方依赖：
//   - crypto/aes, crypto/cipher, crypto/hkdf, crypto/rand, crypto/sha256
//   - encoding/base64, encoding/hex, encoding/json
//
// 协议概要（每条消息一行 JSON）：
//
//	A -> B  hello: {type:"s2s_hello", v:1, ts, nonce, salt}
//	        salt = AES-256-GCM(RootKey, nonce, NewSalt, aad="...|hello|ts")
//	B -> A  ack:   {type:"s2s_ack", ts, nonce, data}
//	        data = AES-256-GCM(SessionKey(s2c), nonce, "s2s_ok|<fp>", aad="...|ack|ts")
//	A <-> B data:  {type:"s2s_data", ts, nonce, data}
//	        data = AES-256-GCM(SessionKey(direction), nonce, payload, aad="...|data|ts")
//
// SessionKey 由 HKDF-SHA256(RootKey, salt=NewSalt, info="neura-s2s-v1/session") 派生，
// 再按方向派生 c2s / s2c 两把 32 字节子密钥，避免反射攻击。
//
// 接收端对每一帧要求 |now-ts| <= 3s，并把 nonce 记入带 TTL 的进程内 Map；
// 重复 nonce 直接拒绝。这个 Map 代替 Redis，节点重启即清空，零外部服务。
//
// A 方握手是异步状态机：BeginClientHandshake 立即返回，后台 goroutine 推进
// Init -> HelloSent -> Established / Failed；调用方用 Wait/WaitTimeout 取结果，
// 握手期间可以继续做别的事，不会阻塞。
package cluster

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	s2sVersion = 1

	rootKeyLen    = 32
	newSaltLen    = 32
	nonceLen      = 12
	sessionKeyLen = 32

	// MaxClockSkew 是业务帧允许的最大时间偏差，也是规范要求的 3 秒防重放窗口。
	// 它同时用于握手帧，避免重放旧 hello。
	MaxClockSkew = 3 * time.Second

	// defaultReplayTTL 比时间戳窗口更长：即使帧在窗口边缘被接受，nonce 也会
	// 在时间戳失效后继续保留一段时间，防止边缘重放。
	defaultReplayTTL = 2 * MaxClockSkew

	maxHandshakeFrame = 64 << 10 // 64 KiB
	maxSecureFrame    = 8 << 20  // 8 MiB

	ackPrefix = "s2s_ok|"

	frameTypeHello = "s2s_hello"
	frameTypeAck   = "s2s_ack"
	frameTypeData  = "s2s_data"
)

// S2S 层的哨兵错误，便于调用方 errors.Is 判断。
var (
	ErrS2SHandshake = errors.New("s2s: 握手失败")
	ErrS2SReplay    = errors.New("s2s: 重放帧被拒绝")
	ErrS2SStale     = errors.New("s2s: 帧时间戳超出 3 秒窗口")
	ErrS2SFrame     = errors.New("s2s: 非法帧")
)

// ---------------- Root Key ----------------

// RootKey 是线下分发的 32 字节根密钥。它只参与握手，不直接加密业务数据。
type RootKey struct {
	k [rootKeyLen]byte
}

// NewRootKey 从 32 字节原始密钥构造 RootKey。
func NewRootKey(raw []byte) (*RootKey, error) {
	if len(raw) != rootKeyLen {
		return nil, fmt.Errorf("s2s: root key 必须是 %d 字节，实际 %d 字节", rootKeyLen, len(raw))
	}
	k := &RootKey{}
	copy(k.k[:], raw)
	return k, nil
}

// GenerateRootKey 用 crypto/rand 生成一把新的 Root Key（运维线下分发用）。
func GenerateRootKey() (*RootKey, error) {
	var raw [rootKeyLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("s2s: 生成 root key: %w", err)
	}
	return &RootKey{k: raw}, nil
}

// ParseRootKey 解析命令行/配置文件里的 Root Key。
//
// 接受以下形式（前后空白会被忽略）：
//   - 64 个十六进制字符
//   - 32 字节的 base64（标准编码，44 字符）
//   - 带前缀的 "hex:..." / "base64:..."
func ParseRootKey(s string) (*RootKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("s2s: root key 为空")
	}

	var raw []byte
	var err error
	switch {
	case strings.HasPrefix(s, "hex:"):
		raw, err = hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(s, "hex:")))
		if err != nil {
			return nil, fmt.Errorf("s2s: root key 不是合法 hex: %w", err)
		}
	case strings.HasPrefix(s, "base64:"):
		raw, err = base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(s, "base64:")))
		if err != nil {
			return nil, fmt.Errorf("s2s: root key 不是合法 base64: %w", err)
		}
	default:
		// 先按 hex 试（64 字符），再按 base64 试（44 字符）。两者都要求 32 字节。
		if b, e := hex.DecodeString(s); e == nil && len(b) == rootKeyLen {
			raw = b
		} else if b, e := base64.StdEncoding.DecodeString(s); e == nil && len(b) == rootKeyLen {
			raw = b
		} else {
			return nil, fmt.Errorf("s2s: root key 必须是 64 个 hex 字符或 32 字节 base64（可用 `openssl rand -hex 32` 生成）")
		}
	}
	if len(raw) != rootKeyLen {
		return nil, fmt.Errorf("s2s: root key 必须是 %d 字节，实际 %d 字节", rootKeyLen, len(raw))
	}
	return NewRootKey(raw)
}

// Fingerprint 返回用于日志的短指纹，可以安全打印，不会泄露密钥。
func (k *RootKey) Fingerprint() string {
	sum := sha256.Sum256(append([]byte("neura-s2s-v1 root|"), k.k[:]...))
	return hex.EncodeToString(sum[:8])
}

// String 只输出指纹，避免误把密钥写进日志。
func (k *RootKey) String() string { return "rootkey#" + k.Fingerprint() }

// Hex 返回完整 hex 编码。只在明确需要导出密钥时调用。
func (k *RootKey) Hex() string { return hex.EncodeToString(k.k[:]) }

func (k *RootKey) aead() (cipher.AEAD, error) { return newAEAD(k.k[:]) }

// ---------------- 密钥派生 / 会话 ----------------

// deriveSession 用 HKDF-SHA256 从 RootKey + NewSalt 派生会话密钥。
// client=true 表示 A 方（发起端），发送 c2s、接收 s2c；B 方相反。
func deriveSession(root *RootKey, newSalt []byte, client bool) (*Session, error) {
	if root == nil {
		return nil, errors.New("s2s: root key 未配置")
	}
	if len(newSalt) != newSaltLen {
		return nil, fmt.Errorf("%w: NewSalt 长度 = %d，want %d", ErrS2SFrame, len(newSalt), newSaltLen)
	}

	prk, err := hkdf.Extract(sha256.New, root.k[:], newSalt)
	if err != nil {
		return nil, fmt.Errorf("s2s: HKDF-Extract: %w", err)
	}
	sessionKey, err := hkdf.Expand(sha256.New, prk, "neura-s2s-v1/session", sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("s2s: HKDF-Expand session: %w", err)
	}
	c2s, err := hkdf.Expand(sha256.New, sessionKey, "neura-s2s-v1/c2s", sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("s2s: HKDF-Expand c2s: %w", err)
	}
	s2c, err := hkdf.Expand(sha256.New, sessionKey, "neura-s2s-v1/s2c", sessionKeyLen)
	if err != nil {
		return nil, fmt.Errorf("s2s: HKDF-Expand s2c: %w", err)
	}

	sendKey, recvKey := c2s, s2c
	if !client {
		sendKey, recvKey = s2c, c2s
	}
	sess, err := newSession(sendKey, recvKey, newSalt)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// Session 是一条已建立连接上的收发密钥材料。send/recv 是两把方向不同的
// AES-256-GCM AEAD，明文 AAD 绑定帧类型和时间戳。
type Session struct {
	send cipher.AEAD
	recv cipher.AEAD

	created time.Time
	saltFP  string
}

func newSession(sendKey, recvKey, salt []byte) (*Session, error) {
	send, err := newAEAD(sendKey)
	if err != nil {
		return nil, err
	}
	recv, err := newAEAD(recvKey)
	if err != nil {
		return nil, err
	}
	return &Session{
		send:    send,
		recv:    recv,
		created: time.Now(),
		saltFP:  fingerprint("neura-s2s-v1 salt|", salt),
	}, nil
}

// Fingerprint 是会话的短指纹（双方一致），用于日志与 ack 校验。
func (s *Session) Fingerprint() string { return s.saltFP }

// Created 返回会话建立时间。
func (s *Session) Created() time.Time { return s.created }

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("s2s: AES-256 需要 32 字节密钥，实际 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("s2s: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("s2s: cipher.NewGCM: %w", err)
	}
	return gcm, nil
}

// sealAEAD 生成随机 96-bit nonce，并用 aad 认证明文。
func sealAEAD(aead cipher.AEAD, plaintext, aad []byte) (nonce, sealed []byte, err error) {
	nonce = make([]byte, nonceLen)
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("s2s: 生成 nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

func openAEAD(aead cipher.AEAD, nonce, sealed, aad []byte) ([]byte, error) {
	if len(nonce) != nonceLen {
		return nil, fmt.Errorf("%w: nonce 长度 = %d，want %d", ErrS2SFrame, len(nonce), nonceLen)
	}
	pt, err := aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: AES-256-GCM 认证失败: %v", ErrS2SFrame, err)
	}
	return pt, nil
}

func fingerprint(domain string, b []byte) string {
	sum := sha256.Sum256(append([]byte(domain), b...))
	return hex.EncodeToString(sum[:8])
}

// ---------------- 时间戳 / 防重放 ----------------

func checkTimestamp(ts int64, now time.Time, skew time.Duration) error {
	if ts <= 0 {
		return fmt.Errorf("%w: 缺少时间戳", ErrS2SStale)
	}
	d := now.Sub(time.Unix(ts, 0))
	if d > skew || d < -skew {
		return fmt.Errorf("%w（偏差 %s）", ErrS2SStale, d.Round(time.Millisecond))
	}
	return nil
}

// ReplayCache 是进程内的 nonce 去重表，带 TTL，替代 Redis。
// 零外部依赖：只在 Go 内存里保存 nonce → 过期时间。
type ReplayCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]time.Time
	lastGC  time.Time
	now     func() time.Time // 测试可注入
}

// NewReplayCache 创建防重放表。ttl <= 0 时使用 6 秒（2 倍时间戳窗口）。
func NewReplayCache(ttl time.Duration) *ReplayCache {
	if ttl <= 0 {
		ttl = defaultReplayTTL
	}
	return &ReplayCache{
		ttl:     ttl,
		entries: make(map[string]time.Time),
		now:     time.Now,
	}
}

// CheckAndAdd 先校验 |now-ts| <= 3s，再原子地检查/登记 nonce。
// key 建议带方向/帧类型前缀（如 "h:..." / "d:..."），避免命名空间冲突。
func (c *ReplayCache) CheckAndAdd(ts int64, key string) error {
	now := c.now()
	if err := checkTimestamp(ts, now, MaxClockSkew); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("%w: 空 nonce", ErrS2SFrame)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.gcLocked(now)
	if exp, ok := c.entries[key]; ok && exp.After(now) {
		return fmt.Errorf("%w: nonce=%s", ErrS2SReplay, shortNonce(key))
	}
	c.entries[key] = now.Add(c.ttl)
	return nil
}

func (c *ReplayCache) gcLocked(now time.Time) {
	// 最多每秒清理一次，常态检查是 O(1)；过期条目即使没被清理也不会命中。
	if !c.lastGC.IsZero() && now.Sub(c.lastGC) < time.Second {
		return
	}
	c.lastGC = now
	for k, exp := range c.entries {
		if !exp.After(now) {
			delete(c.entries, k)
		}
	}
}

// Len 返回当前保留的 nonce 数（测试/观测用）。
func (c *ReplayCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// TTL 返回条目保留时长。
func (c *ReplayCache) TTL() time.Duration { return c.ttl }

func shortNonce(s string) string {
	s = strings.TrimPrefix(s, "d:")
	s = strings.TrimPrefix(s, "h:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------------- wire 帧 ----------------

type helloFrame struct {
	Type    string `json:"type"`
	Version int    `json:"v"`
	TS      int64  `json:"ts"`
	Nonce   string `json:"nonce"`
	Salt    string `json:"salt"`
	Client  string `json:"client,omitempty"`
}

type wireFrame struct {
	Type  string `json:"type"`
	TS    int64  `json:"ts"`
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

func s2sAAD(kind string, ts int64) []byte {
	return []byte("neura-s2s-v1|" + kind + "|" + strconv.FormatInt(ts, 10))
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: base64 解码: %v", ErrS2SFrame, err)
	}
	return b, nil
}

// readLine 读取一行，并限制最大长度，避免对端用超长行打爆内存。
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			return nil, fmt.Errorf("%w: 帧超过 %d 字节", ErrS2SFrame, max)
		}
		buf = append(buf, chunk...)
		switch {
		case err == nil:
			return bytes.TrimSpace(buf), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if len(buf) == 0 {
				return nil, err
			}
			return nil, fmt.Errorf("%w: 读取帧: %v", ErrS2SFrame, err)
		}
	}
}

// sealFrame 生成一个业务 data 帧。测试可以传入任意 ts 验证时间戳拒绝逻辑。
func (s *Session) sealFrame(payload []byte, ts time.Time) (wireFrame, error) {
	nonce, sealed, err := sealAEAD(s.send, payload, s2sAAD("data", ts.Unix()))
	if err != nil {
		return wireFrame{}, err
	}
	return wireFrame{
		Type:  frameTypeData,
		TS:    ts.Unix(),
		Nonce: b64(nonce),
		Data:  b64(sealed),
	}, nil
}

// WriteFrame 用当前时间密封一帧业务数据并写入 w（含换行）。
func (s *Session) WriteFrame(w io.Writer, payload []byte) error {
	return s.writeFrameAt(w, payload, time.Now())
}

// writeFrameAt 用指定时间戳写帧（测试用来构造过期帧）。
func (s *Session) writeFrameAt(w io.Writer, payload []byte, ts time.Time) error {
	f, err := s.sealFrame(payload, ts)
	if err != nil {
		return err
	}
	line, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("s2s: 编码帧: %w", err)
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("s2s: 写帧: %w", err)
	}
	return nil
}

// ReadFrame 读取并解密一帧业务数据。replay 非空时做 nonce 去重 + 3 秒时间戳校验；
// 为 nil 时至少做时间戳校验（客户端单次响应场景）。
func (s *Session) ReadFrame(r *bufio.Reader, replay *ReplayCache) ([]byte, error) {
	line, err := readLine(r, maxSecureFrame)
	if err != nil {
		return nil, err
	}
	var f wireFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, fmt.Errorf("%w: 解析帧: %v", ErrS2SFrame, err)
	}
	if f.Type != frameTypeData {
		return nil, fmt.Errorf("%w: 期望 %q，实际 %q", ErrS2SFrame, frameTypeData, f.Type)
	}
	if replay != nil {
		if err := replay.CheckAndAdd(f.TS, "d:"+f.Nonce); err != nil {
			return nil, err
		}
	} else if err := checkTimestamp(f.TS, time.Now(), MaxClockSkew); err != nil {
		return nil, err
	}

	nonce, err := unb64(f.Nonce)
	if err != nil {
		return nil, err
	}
	sealed, err := unb64(f.Data)
	if err != nil {
		return nil, err
	}
	return openAEAD(s.recv, nonce, sealed, s2sAAD("data", f.TS))
}

// ---------------- B 方（服务端）握手 ----------------

// ServerHandshake 在已接受的连接上完成 B 方握手：
// 读取 hello → 用 RootKey 解开 NewSalt → HKDF 派生会话 → 回加密 ack。
// 成功后返回 *Session，后续业务帧用 ReadFrame/WriteFrame。
func ServerHandshake(conn net.Conn, br *bufio.Reader, root *RootKey, replay *ReplayCache, timeout time.Duration) (*Session, error) {
	if root == nil {
		return nil, fmt.Errorf("%w: 未配置 root key", ErrS2SHandshake)
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if br == nil {
		br = bufio.NewReader(conn)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("%w: 设置握手超时: %v", ErrS2SHandshake, err)
	}

	line, err := readLine(br, maxHandshakeFrame)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 hello: %v", ErrS2SHandshake, err)
	}
	var hello helloFrame
	if err := json.Unmarshal(line, &hello); err != nil {
		return nil, fmt.Errorf("%w: 解析 hello: %v", ErrS2SHandshake, err)
	}
	if hello.Type != frameTypeHello {
		return nil, fmt.Errorf("%w: 期望 hello，实际 %q", ErrS2SHandshake, hello.Type)
	}
	if hello.Version != s2sVersion {
		return nil, fmt.Errorf("%w: 协议版本 = %d，want %d", ErrS2SHandshake, hello.Version, s2sVersion)
	}

	// 先做便宜的时间戳检查；nonce 登记必须等 Root Key 认证通过之后，
	// 否则未认证连接可以灌满 TTL Map（内存 DoS / 挤掉合法 nonce）。
	if err := checkTimestamp(hello.TS, time.Now(), MaxClockSkew); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrS2SHandshake, err)
	}

	nonce, err := unb64(hello.Nonce)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrS2SHandshake, err)
	}
	sealed, err := unb64(hello.Salt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrS2SHandshake, err)
	}
	rootAEAD, err := root.aead()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrS2SHandshake, err)
	}
	newSalt, err := openAEAD(rootAEAD, nonce, sealed, s2sAAD("hello", hello.TS))
	if err != nil {
		return nil, fmt.Errorf("%w: 无法用 root key 解开 NewSalt: %v", ErrS2SHandshake, err)
	}

	sess, err := deriveSession(root, newSalt, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrS2SHandshake, err)
	}
	if replay != nil {
		// Root Key 认证已通过，登记 hello nonce，拒绝重放旧握手。
		if err := replay.CheckAndAdd(hello.TS, "h:"+hello.Nonce); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrS2SHandshake, err)
		}
	}
	ackTS := time.Now()
	ackNonce, ackSealed, err := sealAEAD(sess.send, []byte(ackPrefix+sess.Fingerprint()), s2sAAD("ack", ackTS.Unix()))
	if err != nil {
		return nil, fmt.Errorf("%w: 生成 ack: %v", ErrS2SHandshake, err)
	}
	ack := wireFrame{
		Type:  frameTypeAck,
		TS:    ackTS.Unix(),
		Nonce: b64(ackNonce),
		Data:  b64(ackSealed),
	}
	line, err = json.Marshal(ack)
	if err != nil {
		return nil, fmt.Errorf("%w: 编码 ack: %v", ErrS2SHandshake, err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, fmt.Errorf("%w: 发送 ack: %v", ErrS2SHandshake, err)
	}
	return sess, nil
}

// ---------------- A 方（客户端）异步握手状态机 ----------------

// HandshakeState 是 A 方握手状态。
type HandshakeState int

const (
	HandshakeInit HandshakeState = iota
	HandshakeHelloSent
	HandshakeEstablished
	HandshakeFailed
)

func (s HandshakeState) String() string {
	switch s {
	case HandshakeInit:
		return "Init"
	case HandshakeHelloSent:
		return "HelloSent"
	case HandshakeEstablished:
		return "Established"
	case HandshakeFailed:
		return "Failed"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// ClientHandshake 是 A 方异步握手句柄。BeginClientHandshake 立即返回，
// 状态在后台 goroutine 中推进：Init -> HelloSent -> Established / Failed。
type ClientHandshake struct {
	conn    net.Conn
	br      *bufio.Reader
	root    *RootKey
	timeout time.Duration
	client  string
	logf    func(string, ...any)
	done    chan struct{}

	mu          sync.Mutex
	state       HandshakeState
	sess        *Session
	err         error
	transitions []HandshakeState
}

// BeginClientHandshake 立即返回并启动后台握手，不阻塞调用方。
// conn/br 由调用方持有；超时后调用方应 Close conn 以回收后台 goroutine。
func BeginClientHandshake(conn net.Conn, br *bufio.Reader, root *RootKey, timeout time.Duration, logf func(string, ...any)) *ClientHandshake {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if br == nil {
		br = bufio.NewReader(conn)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	h := &ClientHandshake{
		conn:        conn,
		br:          br,
		root:        root,
		timeout:     timeout,
		logf:        logf,
		done:        make(chan struct{}),
		state:       HandshakeInit,
		transitions: []HandshakeState{HandshakeInit},
	}
	go h.run()
	return h
}

// run 是真正的异步状态机：每次循环只推进一个状态。
func (h *ClientHandshake) run() {
	defer close(h.done)
	for {
		switch h.State() {
		case HandshakeInit:
			if err := h.sendHello(); err != nil {
				h.fail(err)
				return
			}
			h.transition(HandshakeHelloSent)
		case HandshakeHelloSent:
			if err := h.readAck(); err != nil {
				h.fail(err)
				return
			}
			h.transition(HandshakeEstablished)
		default:
			return
		}
	}
}

func (h *ClientHandshake) sendHello() error {
	if h.root == nil {
		return errors.New("root key 未配置")
	}
	if err := h.conn.SetDeadline(time.Now().Add(h.timeout)); err != nil {
		return fmt.Errorf("设置握手超时: %w", err)
	}

	newSalt := make([]byte, newSaltLen)
	if _, err := rand.Read(newSalt); err != nil {
		return fmt.Errorf("生成 NewSalt: %w", err)
	}
	sess, err := deriveSession(h.root, newSalt, true)
	if err != nil {
		return err
	}

	ts := time.Now()
	rootAEAD, err := h.root.aead()
	if err != nil {
		return err
	}
	nonce, sealed, err := sealAEAD(rootAEAD, newSalt, s2sAAD("hello", ts.Unix()))
	if err != nil {
		return err
	}
	hello := helloFrame{
		Type:    frameTypeHello,
		Version: s2sVersion,
		TS:      ts.Unix(),
		Nonce:   b64(nonce),
		Salt:    b64(sealed),
		Client:  h.client,
	}
	line, err := json.Marshal(hello)
	if err != nil {
		return fmt.Errorf("编码 hello: %w", err)
	}
	if _, err := h.conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("发送 hello: %w", err)
	}

	h.mu.Lock()
	h.sess = sess
	h.mu.Unlock()
	h.logf("s2s: hello sent salt_fp=%s", sess.Fingerprint())
	return nil
}

func (h *ClientHandshake) readAck() error {
	line, err := readLine(h.br, maxHandshakeFrame)
	if err != nil {
		return fmt.Errorf("读取 ack: %w", err)
	}
	var f wireFrame
	if err := json.Unmarshal(line, &f); err != nil {
		return fmt.Errorf("解析 ack: %w", err)
	}
	if f.Type != frameTypeAck {
		return fmt.Errorf("期望 ack，实际 %q", f.Type)
	}
	if err := checkTimestamp(f.TS, time.Now(), MaxClockSkew); err != nil {
		return err
	}
	nonce, err := unb64(f.Nonce)
	if err != nil {
		return err
	}
	sealed, err := unb64(f.Data)
	if err != nil {
		return err
	}

	h.mu.Lock()
	sess := h.sess
	h.mu.Unlock()
	if sess == nil {
		return errors.New("会话密钥尚未派生")
	}
	pt, err := openAEAD(sess.recv, nonce, sealed, s2sAAD("ack", f.TS))
	if err != nil {
		return fmt.Errorf("校验 ack: %w", err)
	}
	if string(pt) != ackPrefix+sess.Fingerprint() {
		return errors.New("ack 会话指纹不匹配")
	}
	h.logf("s2s: ack ok salt_fp=%s", sess.Fingerprint())
	return nil
}

func (h *ClientHandshake) transition(s HandshakeState) {
	h.mu.Lock()
	h.state = s
	h.transitions = append(h.transitions, s)
	h.mu.Unlock()
	h.logf("s2s: handshake -> %s", s)
}

func (h *ClientHandshake) fail(err error) {
	if err == nil {
		err = errors.New("unknown handshake error")
	}
	h.mu.Lock()
	h.err = fmt.Errorf("%w: %w", ErrS2SHandshake, err)
	h.state = HandshakeFailed
	h.transitions = append(h.transitions, HandshakeFailed)
	h.mu.Unlock()
	h.logf("s2s: 握手失败: %v", err)
}

// Done 在握手结束（成功或失败）时关闭。
func (h *ClientHandshake) Done() <-chan struct{} { return h.done }

// State 返回当前状态（并发安全）。
func (h *ClientHandshake) State() HandshakeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

// Err 返回握手错误（未失败时为 nil）。
func (h *ClientHandshake) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// Session 返回已建立的会话；未完成时返回 nil。
func (h *ClientHandshake) Session() *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess
}

// Transitions 返回状态迁移历史（测试/观测用）。
func (h *ClientHandshake) Transitions() []HandshakeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HandshakeState(nil), h.transitions...)
}

// Wait 等待握手完成。ctx 取消时返回错误，但后台状态机可能仍在运行，
// 调用方应关闭连接以回收它。
func (h *ClientHandshake) Wait(ctx context.Context) (*Session, error) {
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.err != nil {
			return nil, h.err
		}
		return h.sess, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrS2SHandshake, ctx.Err())
	}
}

// WaitTimeout 是 Wait 的便捷版本。
func (h *ClientHandshake) WaitTimeout(d time.Duration) (*Session, error) {
	if d <= 0 {
		d = h.timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return h.Wait(ctx)
}

// ---------------- 客户端连接 / 调用 ----------------

// SecureConn 是 A 方到某节点的加密连接。DialSecure 返回时握手已在后台开始，
// 调用方可以先做别的事，需要时再用 Session/Call 等待握手结果。
// 一条 SecureConn 只用于一次请求（与现有 JSON-over-TCP 协议一致）。
type SecureConn struct {
	conn    net.Conn
	br      *bufio.Reader
	hs      *ClientHandshake
	timeout time.Duration
}

// DialSecure 建立 TCP 连接并立即返回；握手在后台异步进行。
func DialSecure(addr string, root *RootKey, timeout time.Duration) (*SecureConn, error) {
	if root == nil {
		return nil, errors.New("s2s: root key 未配置")
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	br := bufio.NewReader(conn)
	hs := BeginClientHandshake(conn, br, root, timeout, nil)
	return &SecureConn{conn: conn, br: br, hs: hs, timeout: timeout}, nil
}

// Handshake 返回后台握手句柄，便于观察状态机。
func (c *SecureConn) Handshake() *ClientHandshake { return c.hs }

// Session 等待握手完成并返回会话。
func (c *SecureConn) Session(ctx context.Context) (*Session, error) {
	return c.hs.Wait(ctx)
}

// Call 在已建立（或正在建立）的加密连接上完成一次请求/响应。
func (c *SecureConn) Call(req Request) (Response, error) {
	sess, err := c.hs.WaitTimeout(c.timeout)
	if err != nil {
		return Response{}, err
	}
	if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return Response{}, fmt.Errorf("s2s: 设置请求超时: %w", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("s2s: 编码请求: %w", err)
	}
	if err := sess.WriteFrame(c.conn, payload); err != nil {
		return Response{}, err
	}
	// 响应只有一帧；仍做时间戳 + nonce 校验（本地表，零外部依赖）。
	respPayload, err := sess.ReadFrame(c.br, NewReplayCache(defaultReplayTTL))
	if err != nil {
		return Response{}, fmt.Errorf("s2s: 读取响应: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(respPayload, &resp); err != nil {
		return Response{}, fmt.Errorf("s2s: 解析响应: %w", err)
	}
	return resp, nil
}

// Close 关闭底层连接。
func (c *SecureConn) Close() error { return c.conn.Close() }

// CallSecure 是对 Call 的加密版本：Root Key 握手 → 加密 Request → 解密 Response。
// 握手本身由异步状态机完成，这里只是等待结果。
func CallSecure(addr string, root *RootKey, req Request, timeout time.Duration) (Response, error) {
	sc, err := DialSecure(addr, root, timeout)
	if err != nil {
		return Response{}, err
	}
	defer sc.Close()
	return sc.Call(req)
}
