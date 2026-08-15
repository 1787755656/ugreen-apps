package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"fmt"
)

// qBittorrent 的 WebUI 密码格式（对着 5.2.3 真机生成的配置逐字段验证过）：
//
//	WebUI\Password_PBKDF2="@ByteArray(<base64 盐>:<base64 派生密钥>)"
//
// PBKDF2-HMAC-SHA512、100000 轮、16 字节盐、64 字节密钥。
const (
	pbkdf2Iterations = 100000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 64
)

// pbkdf2SHA512 是 RFC 2898 的 PBKDF2，只用标准库实现，省掉 x/crypto 依赖
// （交叉编译两个架构时零依赖最省事）。
func pbkdf2SHA512(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha512.New, password)
	hashLen := prf.Size()
	blocks := (keyLen + hashLen - 1) / hashLen

	var out []byte
	buf := make([]byte, 4)
	u := make([]byte, 0, hashLen)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u = prf.Sum(u[:0])

		t := make([]byte, hashLen)
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// qbPasswordValue 生成可直接写进 qBittorrent.conf 的值（含外层双引号）。
func qbPasswordValue(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机盐失败: %w", err)
	}
	return qbPasswordValueWithSalt(password, salt), nil
}

func qbPasswordValueWithSalt(password string, salt []byte) string {
	dk := pbkdf2SHA512([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	b64 := base64.StdEncoding
	return fmt.Sprintf("\"@ByteArray(%s:%s)\"", b64.EncodeToString(salt), b64.EncodeToString(dk))
}
