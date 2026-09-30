package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ConfigData 是持久化到 <UGAPP_DATA_DIR>/launcher.json（0600）的内容。
//
// 为什么 DataDir 要存进来而不是每次从环境变量重算：数据库的数据目录一旦定下来就
// 【不能再漂移】。安装参数 MARIADB_DATA_DIR 首次启动时可能还没注入（平台先起服务、
// 2~3 秒后才写 .env），如果每次启动都照环境变量重算，就会出现"第一次落在 A、
// 重启后落在 B"，用户看到的是数据库突然变空。所以初始化那一刻定下来、写进配置，
// 之后只读配置；参数变了只在管理页提示，绝不自动搬。
type ConfigData struct {
	// MySQL 协议监听端口
	Port int `json:"port"`
	// true = 绑 0.0.0.0（局域网可连）；false = 只绑 127.0.0.1
	AllowLAN bool `json:"allow_lan"`
	// root 账号密码。首次初始化时随机生成。
	RootPassword string `json:"root_password"`
	// 用户是否亲手改过密码。放开局域网监听的前置条件。
	PasswordChangedByUser bool `json:"password_changed_by_user"`
	// 数据目录的最终落点，初始化时定下、此后不变。
	DataDir string `json:"data_dir"`
	// 初始化时 MARIADB_DATA_DIR 参数的值，用来在管理页提示"参数改了但数据没搬"。
	DataDirParamAtInit string `json:"data_dir_param_at_init"`
}

// Config 给 ConfigData 套一层锁。
//
// 必须加锁：改配置的是 HTTP 处理协程，读配置的是进程监管协程（生成 my.cnf 时），
// 裸结构体会构成数据竞争。所有访问只有两个入口 —— Snapshot 读、Update 写。
type Config struct {
	mu   sync.RWMutex
	path string
	data ConfigData
}

const (
	defaultPort = 3306
	// 生成密码用的字符集：去掉了 0/O/l/1/I 这些肉眼分不清的字符 ——
	// 这个密码用户是要抄进 Navicat 或配置文件里的。
	passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	passwordLength   = 20
)

func GeneratePassword() (string, error) {
	var sb strings.Builder
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := 0; i < passwordLength; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("生成随机密码失败: %w", err)
		}
		sb.WriteByte(passwordAlphabet[n.Int64()])
	}
	return sb.String(), nil
}

func LoadConfig(dataDir string) (*Config, error) {
	c := &Config{
		path: filepath.Join(dataDir, "launcher.json"),
		data: ConfigData{Port: defaultPort},
	}
	b, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", c.path, err)
	}
	if err := json.Unmarshal(b, &c.data); err != nil {
		return nil, fmt.Errorf("配置 %s 不是合法 JSON: %w", c.path, err)
	}
	if c.data.Port <= 0 {
		c.data.Port = defaultPort
	}
	return c, nil
}

// Snapshot 返回配置的一份拷贝，调用方拿去随便读。
func (c *Config) Snapshot() ConfigData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

// Update 在锁内修改配置并落盘。fn 返回错误时不写盘、也不保留改动。
func (c *Config) Update(fn func(d *ConfigData) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	before := c.data
	if err := fn(&c.data); err != nil {
		c.data = before
		return err
	}
	if err := c.saveLocked(); err != nil {
		c.data = before
		return err
	}
	return nil
}

// saveLocked 原子写盘。数据库密码丢了就只能走重置流程，所以这里不接受"写了一半"。
func (c *Config) saveLocked() error {
	b, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("替换配置失败: %w", err)
	}
	return nil
}

func (d ConfigData) BindAddress() string {
	if d.AllowLAN {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// paramDataDir 读安装参数 MARIADB_DATA_DIR。
//
// 三件事要注意（都是绿联平台的既有行为）：
//   - multi:false 的 path 参数没选时是【字面量 "null"】，不是空串；
//   - 全新安装/升级后的首次启动，参数值和授权目录都还没写好，读到空是正常的；
//   - 用 LookupEnv 而不是判空字符串，"没设置"和"设了空值"要分得开。
func paramDataDir() string {
	v, ok := os.LookupEnv("MARIADB_DATA_DIR")
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" || v == "null" {
		return ""
	}
	return v
}

// ResolveDataDir 决定数据库数据目录，只在【尚未初始化】时才真正做选择。
func (c *Config) ResolveDataDir(p *Paths) (string, error) {
	if d := c.Snapshot(); d.DataDir != "" {
		return d.DataDir, nil
	}

	param := paramDataDir()
	chosen := ""
	if param != "" {
		// 参数指向的是用户选的文件夹本身，数据放进它下面的子目录，
		// 免得把一堆 ibdata / ib_logfile 直接铺在用户的共享文件夹根上。
		candidate := filepath.Join(param, "mariadb-data")
		if err := os.MkdirAll(candidate, 0o750); err == nil {
			chosen = candidate
		} else {
			logf("安装参数指定的目录 %s 不可写（%v），改用应用数据目录", param, err)
		}
	}
	if chosen == "" {
		chosen = filepath.Join(p.Data, "data")
		if err := os.MkdirAll(chosen, 0o750); err != nil {
			return "", fmt.Errorf("创建数据目录 %s 失败: %w", chosen, err)
		}
	}

	err := c.Update(func(d *ConfigData) error {
		d.DataDir = chosen
		d.DataDirParamAtInit = param
		return nil
	})
	if err != nil {
		return "", err
	}
	return chosen, nil
}

// DataDirParamChanged 判断"用户后来改了安装参数，但数据还在老地方"。
// 只提示、不自动搬 —— 搬数据库是有风险的操作，必须由人来决定。
func (c *Config) DataDirParamChanged() (bool, string) {
	now := paramDataDir()
	if now == "" || now == c.Snapshot().DataDirParamAtInit {
		return false, ""
	}
	return true, now
}
