package main

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	pbkdf2Iters   = 120000
	sessionTTL    = 30 * 24 * time.Hour
	sessionCookie = "frpmgr_session"
)

// DefaultPassword 出厂管理密码:固定值而非随机串,便于写进应用简介让用户
// 装上就能进页面(随机密码要求用户先去翻数据目录,手机端根本翻不到)。
// 首次登录后页面顶部会提示改成自己的密码。
const DefaultPassword = "frpadmin"

// manager.json 持久化内容
type managerState struct {
	PasswordSalt string `json:"password_salt"` // base64
	PasswordHash string `json:"password_hash"` // base64
	SessionKey   string `json:"session_key"`   // base64,HMAC 签名钥
	FrpcEnabled  bool   `json:"frpc_enabled"`
	FrpsEnabled  bool   `json:"frps_enabled"`
	// PasswordIsDefault 仍是出厂密码时置 true:页面据此弹"请改密码"提示。
	// 老版本 manager.json 没这个字段 → 零值 false,不会误提示。
	PasswordIsDefault bool `json:"password_is_default"`
}

type store struct {
	mu   sync.Mutex
	path string
	st   managerState
}

// loadOrCreateStore 读取 manager.json;不存在则用出厂密码初始化。
// firstBoot=true 表示刚生成,调用方负责写凭据说明.txt。
func loadOrCreateStore(path string) (*store, bool) {
	s := &store{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.st); err == nil && s.st.PasswordHash != "" {
			return s, false
		}
		log.Printf("manager.json 缺失或损坏,重新初始化(旧文件保留为 manager.json.bak)")
		_ = os.Rename(path, path+".bak")
	}
	if err := s.resetPassword(DefaultPassword); err != nil {
		log.Fatalf("初始化管理密码失败: %v", err)
	}
	s.st.PasswordIsDefault = true
	key, err := randomToken(32)
	if err != nil {
		log.Fatalf("生成会话密钥失败: %v", err)
	}
	s.st.SessionKey = key
	if err := s.saveLocked(); err != nil {
		log.Fatalf("写入 manager.json 失败: %v", err)
	}
	return s, true
}

func (s *store) frpcEnabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.st.FrpcEnabled }
func (s *store) frpsEnabled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.st.FrpsEnabled }

// usingDefaultPassword 是否仍在用出厂密码(页面提示条用)
func (s *store) usingDefaultPassword() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.PasswordIsDefault
}

// setEnabled 持久化某服务的启用开关
func (s *store) setEnabled(name string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch name {
	case "frpc":
		s.st.FrpcEnabled = on
	case "frps":
		s.st.FrpsEnabled = on
	}
	if err := s.saveLocked(); err != nil {
		log.Printf("保存启用状态失败: %v", err)
	}
}

func (s *store) setPassword(newPw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.resetPassword(newPw); err != nil {
		return err
	}
	s.st.PasswordIsDefault = newPw == DefaultPassword
	// 必须落盘:只改内存的话,管理壳重启后密码会回滚成旧值
	return s.saveLocked()
}

func (s *store) resetPassword(pw string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iters, 32)
	if err != nil {
		return err
	}
	s.st.PasswordSalt = base64.StdEncoding.EncodeToString(salt)
	s.st.PasswordHash = base64.StdEncoding.EncodeToString(hash)
	return nil
}

func (s *store) saveLocked() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, b, 0o600)
}

// checkPassword 常量时间比较
func (s *store) checkPassword(pw string) bool {
	s.mu.Lock()
	saltB64, hashB64 := s.st.PasswordSalt, s.st.PasswordHash
	s.mu.Unlock()
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iters, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---------- 会话令牌(HMAC,无状态,重启不丢) ----------

func (s *store) newSessionToken() (string, error) {
	s.mu.Lock()
	keyB64 := s.st.SessionKey
	s.mu.Unlock()
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return "", err
	}
	exp := uint64(time.Now().Add(sessionTTL).Unix())
	var expBuf [8]byte
	binary.BigEndian.PutUint64(expBuf[:], exp)
	mac := hmac.New(sha256.New, key)
	mac.Write(expBuf[:])
	return fmt.Sprintf("%x.%s", exp, base64.RawURLEncoding.EncodeToString(mac.Sum(nil))), nil
}

func (s *store) validSessionToken(tok string) bool {
	s.mu.Lock()
	keyB64 := s.st.SessionKey
	s.mu.Unlock()
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return false
	}
	var exp uint64
	var sigB64 string
	if n, err := fmt.Sscanf(tok, "%x.%s", &exp, &sigB64); err != nil || n != 2 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return false
	}
	var expBuf [8]byte
	binary.BigEndian.PutUint64(expBuf[:], exp)
	mac := hmac.New(sha256.New, key)
	mac.Write(expBuf[:])
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return false
	}
	// 溢出防护:exp 极大值会通过 Sscanf %x 吗?uint64 上限内,time 比较即可
	if exp > uint64(math.MaxInt64) {
		return false
	}
	return time.Now().Unix() < int64(exp)
}

// ---------- 登录限流(单机内存版:连续失败锁 60s) ----------

type limiter struct {
	mu      sync.Mutex
	fails   map[string]*failState
	max     int
	lockFor time.Duration
}

type failState struct {
	count int
	until time.Time
}

func newLimiter() *limiter {
	return &limiter{fails: map[string]*failState{}, max: 5, lockFor: time.Minute}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.fails[key]
	return st == nil || time.Now().After(st.until)
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.fails[key]
	if st == nil {
		st = &failState{}
		l.fails[key] = st
	}
	st.count++
	if st.count >= l.max {
		st.until = time.Now().Add(l.lockFor)
		st.count = 0
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// ---------- 小工具 ----------

// randomToken 返回 n 字节随机数的 hex 字符串
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeCredentialsFile 首启落一份说明(与 CLIProxyAPI 凭据说明.txt 惯例一致)
func writeCredentialsFile() {
	content := `FRP 内网穿透 —— 管理凭据
=================================

默认管理密码: ` + DefaultPassword + `
(出厂固定值;登录后在页面右上角「修改密码」改成自己的密码)

忘记密码怎么办:删除本目录下的 manager.json 再重启应用,
密码恢复为默认值(两个服务的启用状态会一并重置)。

管理地址: 从 UGOS 桌面/应用中心打开本应用;
        局域网也可直接访问 http://<NAS_IP>:` + fmt.Sprintf("%d", defaultPort) + `

配置文件(应用数据目录,网页里也可直接编辑):
  frps.toml   服务端配置
  frpc.toml   客户端配置

本文件只在首次启动时生成;改完密码后可自行删除。
`
	p := filepath.Join(dataDir, "凭据说明.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		log.Printf("写入凭据说明.txt 失败: %v", err)
	}
}

var errNotExist = errors.New("not exist")

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
