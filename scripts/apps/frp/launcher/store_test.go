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

// newTestStore 起一个首启状态的 store,返回它和出厂密码
func newTestStore(t *testing.T) (*store, string) {
	t.Helper()
	st, firstBoot := loadOrCreateStore(filepath.Join(t.TempDir(), "manager.json"))
	if !firstBoot {
		t.Fatalf("首次初始化应返回 firstBoot=true")
	}
	return st, DefaultPassword
}

func TestDefaultPasswordOnFirstBoot(t *testing.T) {
	st, pw := newTestStore(t)
	if pw != "frpadmin" {
		t.Fatalf("出厂密码应为 frpadmin,实际 %q", pw)
	}
	if !st.checkPassword(pw) {
		t.Fatalf("出厂密码校验失败")
	}
	if !st.usingDefaultPassword() {
		t.Fatalf("首启应标记为仍在使用默认密码")
	}
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
	if st.usingDefaultPassword() {
		t.Fatalf("改过密码后不应再标记为默认密码")
	}
}

// 改密码必须落盘:早期版本 setPassword 只改内存,重启后密码会回滚
func TestSetPasswordPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	st, _ := loadOrCreateStore(path)
	if err := st.setPassword("NewPassw0rd9"); err != nil {
		t.Fatalf("改密码失败: %v", err)
	}
	st2, first2 := loadOrCreateStore(path)
	if first2 {
		t.Fatalf("重开不应重新初始化")
	}
	if st2.checkPassword(DefaultPassword) {
		t.Fatalf("重启后出厂密码不应再生效(说明没落盘)")
	}
	if !st2.checkPassword("NewPassw0rd9") {
		t.Fatalf("重启后新密码应仍有效")
	}
	if st2.usingDefaultPassword() {
		t.Fatalf("重启后不应再标记为默认密码")
	}
}

func TestLoadOrCreatePreservesPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	_, first := loadOrCreateStore(path)
	if !first {
		t.Fatalf("首次应为 firstBoot")
	}
	st2, first2 := loadOrCreateStore(path)
	if first2 {
		t.Fatalf("二次加载不应重新初始化")
	}
	if !st2.checkPassword(DefaultPassword) {
		t.Fatalf("重启后密码应保持一致")
	}
}

func TestCorruptedStoreReinitializes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	if err := os.WriteFile(path, []byte("\x00garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, first := loadOrCreateStore(path)
	if !first {
		t.Fatalf("损坏文件应触发重新初始化")
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("旧文件应保留为 .bak")
	}
	if !st.checkPassword(DefaultPassword) {
		t.Fatalf("重新初始化后应回到出厂密码")
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
	// 空令牌(没带 Cookie 也没带 Authorization 头)必须无效
	if st.validSessionToken("") {
		t.Fatalf("空令牌应无效")
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
