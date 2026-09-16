package main

// UGOS 登录验证客户端 —— 给"无桥环境"（手机浏览器等）的会话开通用。
//
// 【背景】inner 应用的 /api 认证靠网关注入 Ugreen-User-*，而注入的前提是
// 前端带 Ugreen-Ttk。Ttk 由桌面 JSSDK 的 getThirdToken 取得——手机浏览器
// 里没有宿主桥，拿不到。桌面端靠桥，移动端只能退而求其次：
// 用户直接输 NAS 账号密码，管理壳【代为到本机 UGOS 登录接口验密】，
// 验过才签会话 Cookie（session.go）。密码只转发给 UGOS 自己的登录接口，
// 不落盘、不写日志。
//
// 接口细节（ug-message-bots-native 真机逆向结论）：
//   POST /ugreen/v1/verify/check  {"username":...}   → RSA 公钥在【响应头 x-rsa-token】
//   POST /ugreen/v1/verify/login  {"username","password":RSA加密后base64,...} → data.token
//   - 公钥 PEM 头写 PKCS#1，内容实际是 PKIX/SPKI：先按 PKIX 解析，失败退回 PKCS#1
//   - 加密是 RSA PKCS#1 v1.5（不是 OAEP）
//   - 用户名字面 "admin" 的账号走不加密分支（密码明文字段）
//   - 所有请求都要带 client-version 头（值不参与判断，缺了部分接口静默返回残缺数据）
//   - 开了两步验证的账号：data.token 为空 + enable_otp=true —— 明确报错让用户改用桌面端

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const ugosClientVersion = "78291"

type ugosLoginClient struct {
	base string // 如 https://127.0.0.1:9443
	http *http.Client
}

func newUgosLoginClient(base string) *ugosLoginClient {
	return &ugosLoginClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				// 自签名证书：入口是写死的本机回环地址，验证证书链没有意义
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
	}
}

func (c *ugosLoginClient) post(path string, body any) (*http.Response, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest("POST", c.base+path, bytes.NewReader(buf))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("client-version", ugosClientVersion)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, data, err
}

type ugosEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Login 验证一组 UGOS 账号密码；成功返回（空的）会话语义，失败返回可给用户看的话。
func (c *ugosLoginClient) Login(username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("用户名和密码不能为空")
	}

	// 1. verify/check：拿 RSA 公钥（在响应头 x-rsa-token）
	resp, data, err := c.post("/ugreen/v1/verify/check", map[string]string{"username": username})
	if err != nil {
		return fmt.Errorf("无法连接 UGOS 登录接口：%v", err)
	}
	var env ugosEnvelope
	_ = json.Unmarshal(data, &env)
	if resp.StatusCode != http.StatusOK || (env.Code != 0 && env.Code != 200) {
		return fmt.Errorf("UGOS 登录接口返回异常（HTTP %d code %d）%s", resp.StatusCode, env.Code, env.Message)
	}
	pemB64 := resp.Header.Get("x-rsa-token")
	if pemB64 == "" && username != "admin" {
		return fmt.Errorf("UGOS 登录接口没有返回公钥，无法加密密码")
	}

	// 2. 加密密码：字面 admin 走不加密分支；其余 RSA PKCS#1 v1.5
	encPassword := password
	if username != "admin" {
		pub, err := parseUgosPublicKey(pemB64)
		if err != nil {
			return fmt.Errorf("解析 UGOS 公钥失败：%v", err)
		}
		enc, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(password))
		if err != nil {
			return fmt.Errorf("加密密码失败：%v", err)
		}
		encPassword = base64.StdEncoding.EncodeToString(enc)
	}

	// 3. verify/login
	resp, data, err = c.post("/ugreen/v1/verify/login", map[string]any{
		"username":  username,
		"password":  encPassword,
		"keepalive": true,
		"otp":       true,
		"is_simple": false,
	})
	if err != nil {
		return fmt.Errorf("无法连接 UGOS 登录接口：%v", err)
	}
	env = ugosEnvelope{}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("UGOS 登录接口响应异常（HTTP %d）", resp.StatusCode)
	}
	if env.Code != 0 && env.Code != 200 {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("code %d", env.Code)
		}
		// 1010/1003 之类给不出更多上下文，原样带上
		return fmt.Errorf("UGOS 登录失败：%s", msg)
	}
	var d struct {
		Token     string `json:"token"`
		EnableOtp bool   `json:"enable_otp"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &d)
	}
	if d.EnableOtp || d.Token == "" {
		return fmt.Errorf("该账号开启了两步验证（或未返回令牌），无法在移动端登录；请用未开两步验证的账号，或在电脑端使用")
	}
	return nil
}

// parseUgosPublicKey 先按 PKIX/SPKI 解析（实际编码），失败退回 PEM 头声明的 PKCS#1。
func parseUgosPublicKey(pemB64 string) (*rsa.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pemB64))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("不是 PEM")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("公钥不是 RSA")
	}
	key, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("PKIX/PKCS#1 都解析失败")
	}
	return key, nil
}
