package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*store, string) {
	t.Helper()
	st, pw, firstBoot := loadOrCreateStore(filepath.Join(t.TempDir(), "manager.json"))
	if !firstBoot || pw == "" {
		t.Fatalf("首次初始化应返回明文密码")
	}
	return st, pw
}

func TestPasswordRoundtrip(t *testing.T) {
	st, pw := newTestStore(t)
	if !st.checkPassword(pw) {
		t.Fatalf("正确密码校验失败")
	}
	if st.checkPassword(pw + "x") {
		t.Fatalf("错误密码不应通过")
	}
	if err := st.setPassword("NewPassw0rd9"); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}
	if st.checkPassword(pw) {
		t.Fatalf("旧密码应失效")
	}
	if !st.checkPassword("NewPassw0rd9") {
		t.Fatalf("新密码应通过")
	}
}

func TestLoadOrCreatePreservesPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	st1, pw1, first := loadOrCreateStore(path)
	if !first {
		t.Fatalf("首次应为 firstBoot")
	}
	st2, pw2, first2 := loadOrCreateStore(path)
	if first2 {
		t.Fatalf("二次加载不应重新初始化")
	}
	if pw2 != "" {
		t.Fatalf("非首启不应返回明文密码")
	}
	if !st2.checkPassword(pw1) {
		t.Fatalf("重启后密码应保持一致")
	}
	_ = st1
}

func TestCorruptedStoreReinitializes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	if err := os.WriteFile(path, []byte("\x00garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, pw, first := loadOrCreateStore(path)
	if !first || pw == "" {
		t.Fatalf("损坏文件应触发重新初始化")
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("旧文件应保留为 .bak")
	}
	if !st.checkPassword(pw) {
		t.Fatalf("重新初始化后密码校验应通过")
	}
}

func TestSessionToken(t *testing.T) {
	st, _ := newTestStore(t)
	tok, err := st.newSessionToken()
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if !st.validSessionToken(tok) {
		t.Fatalf("新令牌应有效")
	}
	// 篡改签名:换掉最后一段
	if st.validSessionToken(tok[:len(tok)-4]+"AAAA") {
		t.Fatalf("篡改后的令牌应无效")
	}
	// 过期令牌:用同一把 key 手工签一个过去的时间戳
	key, err := base64.StdEncoding.DecodeString(st.st.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	exp := uint64(time.Now().Add(-time.Hour).Unix())
	var expBuf [8]byte
	binary.BigEndian.PutUint64(expBuf[:], exp)
	mac := hmac.New(sha256.New, key)
	mac.Write(expBuf[:])
	expired := fmt.Sprintf("%x.%s", exp, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	if st.validSessionToken(expired) {
		t.Fatalf("过期令牌应无效")
	}
}

func TestLimiterLockout(t *testing.T) {
	l := newLimiter()
	const ip = "203.0.113.10"
	for i := 0; i < 5; i++ {
		if !l.allow(ip) {
			t.Fatalf("第 %d 次失败前不应锁定", i+1)
		}
		l.fail(ip)
	}
	if l.allow(ip) {
		t.Fatalf("连续 5 次失败后应锁定")
	}
	l.reset(ip)
	if !l.allow(ip) {
		t.Fatalf("reset 后应放行")
	}
}

func TestGenPassword(t *testing.T) {
	const charset = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		pw := genPassword()
		if len(pw) != 12 {
			t.Fatalf("密码长度应为 12: %q", pw)
		}
		for _, c := range pw {
			if !containsRune(charset, c) {
				t.Fatalf("密码含非法字符 %q: %q", c, pw)
			}
		}
		seen[pw] = true
	}
	if len(seen) < 20 {
		t.Fatalf("20 次生成出现重复,随机性异常")
	}
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}
