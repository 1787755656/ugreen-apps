package main

// 手机端 HTTP 入口的凭据加密通道。
//
// 【为什么需要它】手机端打开 inner 应用时落在浏览器的【明文 HTTP】入口上
// （网关给每对 10015/10016 端口，客户端拿 portInfo 自己拼 URL；实测跳转
// 用的是 http 端口）。原来凭据分支一刀切"仅 HTTPS"，手机上登录表单提交
// 直接 403 —— 这是"手机端打开界面用不了"的主根因。
//
// 放开 HTTP 又不能让密码裸奔。绿联平台自己就是这个处境：HTTP 9999 的
// API 靠应用层 AES-GCM 替代 TLS。这里同水位处理：管理壳出【一次性】
// RSA 公钥（内存里，10 分钟过期），前端加密密码后提交，私钥只解一次——
// 抓包者拿到的密文没有对应私钥，密文本身用后即废，没有重放窗口。
// HTTPS 入口照旧接受明文（行为不变），也接受密文。
//
// 【为什么是 RSA PKCS#1 v1.5】前端是无构建链的原生 JS，iOS 的 WebCrypto
// 在非安全上下文（http 页面）里不给 crypto.subtle，纯 JS 的 PKCS#1 v1.5
// 加密（BigInt modpow，e=65537）十几行就能写对；Go 侧 rsa.DecryptPKCS1v15
// 是标准库。与 UGOS 自己 verify/login 的加密方式也一致。

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	credKeyBits  = 2048
	credKeyTTL   = 10 * time.Minute
	maxCredKeys  = 16 // 防内存撑爆；正常用户一次登录只用一把
	credNonceLen = 16
)

type credKey struct {
	priv    *rsa.PrivateKey
	expires time.Time
}

type loginKeyStore struct {
	mu   sync.Mutex
	keys map[string]credKey
}

func newLoginKeyStore() *loginKeyStore {
	return &loginKeyStore{keys: map[string]credKey{}}
}

// issue 生成一把新的一次性密钥对，返回 key_id 和公钥的 N/E（base64）。
// 前端拿 N/E 做 BigInt 加密，不需要解析 DER/PEM。
func (k *loginKeyStore) issue() (id, nB64, eB64 string, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, credKeyBits)
	if err != nil {
		return "", "", "", err
	}
	nonce := make([]byte, credNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", "", err
	}
	id = hex.EncodeToString(nonce)

	k.mu.Lock()
	defer k.mu.Unlock()
	k.gcLocked()
	if len(k.keys) >= maxCredKeys {
		k.keys = map[string]credKey{} // 粗暴但无害：最坏结果是页面重取一把
	}
	k.keys[id] = credKey{priv: priv, expires: time.Now().Add(credKeyTTL)}

	return id,
		base64.StdEncoding.EncodeToString(priv.N.Bytes()),
		base64.StdEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes()),
		nil
}

// decrypt 用 key_id 对应的私钥解一次密文。无论成败，密钥用过即删——
// 同一段密文没有第二次提交的机会。
func (k *loginKeyStore) decrypt(id string, ct []byte) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.keys[id]
	if !ok {
		return nil, false
	}
	delete(k.keys, id)
	if time.Now().After(key.expires) {
		return nil, false
	}
	pt, err := rsa.DecryptPKCS1v15(rand.Reader, key.priv, ct)
	if err != nil {
		return nil, false
	}
	return pt, true
}

func (k *loginKeyStore) gcLocked() {
	now := time.Now()
	for id, key := range k.keys {
		if now.After(key.expires) {
			delete(k.keys, id)
		}
	}
}

// handleSessionKey 下发一次性公钥。仍然压着"必须来自本机"的闸：
// 手机端的所有请求都经过网关（源地址是 NAS 本机），不受影响；
// 局域网上直连端口的请求拿不到钥匙，也就造不出合法的密文。
func (s *Server) handleSessionKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "密钥下发只接受 GET。")
		return
	}
	id, nB64, eB64, err := s.loginKeys.issue()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "密钥生成失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": id,
		"n":      nB64,
		"e":      eB64,
		"ttl":    int(credKeyTTL / time.Second),
		"note":   "一次性密钥：用后即废，10 分钟过期。密码用 RSA PKCS#1 v1.5 加密后以 password_enc 提交。",
	})
}
