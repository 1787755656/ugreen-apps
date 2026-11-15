package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// settings.toml 生成（仅 data/settings.toml 不存在时写；用户改统计密码/坐标后
// 不被覆盖）。基础设施值（监听端口/资源目录/数据库文件）按本次启动环境钉死；
// 上游 viper 按 SPEEDTEST_ 前缀 env 理论可覆盖，本壳不依赖（toml 即权威，env
// 覆盖仅在键存在于 toml 时生效，属上游行为——记录在开发文档，不作机制依赖）。
//
// 键值依据 upstream v1.1.6 settings.toml / config/config.go：
//   - bind_address="0.0.0.0" 显式 IPv4 全网卡（net.JoinHostPort 拼端口）
//   - database_type="bolt"：bbolt 纯 Go 单文件，双架构零 CGO；落 @appdata
//   - server_lat/lng 保持上游默认 1,1（非零即不出站调 ipinfo.io 定位）
//   - geoip 留空禁用（不随包带 mmdb）
//   - statistics_password 必须非哨兵值 "PASSWORD"，否则 /stats 拒绝出数据
func settingsTomlTemplate(host string, port int, wwwDir, dataDir string) string {
	return fmt.Sprintf(`# LibreSpeed (speedtest-go) 配置 —— 由应用启动器首次启动时生成
# 本文件存在时启动器不再覆盖（可放心修改下列业务项，修改后重启应用生效）。
# 完整键说明见上游仓库 settings.toml。
# 注意：监听端口/数据库路径/前端目录为基础设施项，请勿改动。

# 监听地址与端口（平台探活与浏览器直连都打这个口）
bind_address=%q
listen_port=%d
# 反代协议端口（不用）
proxyprotocol_port=0
# 前端静态资源目录（启动器按版本同步到数据目录，含中文页面与 servername.json）
assets_path=%q

# 服务器坐标（保持默认即不触发出站定位；测速页面已关闭 ISP 信息查询）
server_lat=1
server_lng=1
ipinfo_api_key=""

# 历史记录页 /stats 的查看密码（默认值与应用介绍、数据目录凭据说明.txt 一致；
# 改后重启应用生效。测速页面本身无任何认证）
statistics_password=%q
# 测速记录里 IP 打码（局域网工具默认关闭，保留"哪台设备测的"信息）
redact_ip_addresses=false
# 测速编号混淆（保持明文 ULID，历史页按编号可查）
enable_id_obfuscation=false

# 测速结果历史库：boltdb 单文件，落应用数据目录
database_type="bolt"
database_file=%q
database_hostname=""
database_name=""
database_username=""
database_password=""
database_port=""

# GeoIP 离线库（未随包提供，保持禁用）
# geoip_database_file="country_asn.mmdb"

# TLS/HTTP2（局域网工具走明文 http；如需自担证书再开）
enable_tls=false
enable_http2=false
`, host, port, wwwDir, statsPassword, filepath.Join(dataDir, "speedtest.db"))
}

func ensureSettingsToml(path, host string, port int, wwwDir, dataDir string) error {
	if _, err := os.Stat(path); err == nil {
		log.Printf("settings.toml 已存在，不覆盖（用户改动保留）: %s", path)
		return nil
	}
	if err := os.WriteFile(path, []byte(settingsTomlTemplate(host, port, wwwDir, dataDir)), 0o644); err != nil {
		return err
	}
	log.Printf("已生成 settings.toml（bolt 历史 @ %s，stats 密码见凭据说明）", filepath.Join(dataDir, "speedtest.db"))
	return nil
}

// writeUsageOnce 写凭据说明（仅首启；含 stats 密码与改法、参数说明、安全口径）。
func writeUsageOnce(dataDir string, port int) {
	txt := filepath.Join(dataDir, "凭据说明.txt")
	if _, err := os.Stat(txt); err == nil {
		return
	}
	content := fmt.Sprintf(`LibreSpeed 网速测试 —— 历史记录页密码（本文件由应用自动生成，请勿删除）
生成时间: %s

访问地址（应用中心打开或浏览器直连）:
    http://<NAS的IP>:%d

测速页面: 无需任何登录，局域网内打开即用。
历史记录页（/stats）查看密码: %s

！重要：默认密码与应用介绍里完全相同、公开可见。修改方法：
    1. 编辑应用数据目录下的 settings.toml，把 statistics_password 改成新值；
    2. 在应用中心对该应用执行「停止→启动」。
    修改后本文件不会更新（以 settings.toml 为准）。
    未改密码前请勿将应用端口暴露公网（本应用整体无账号体系，仅建议局域网使用）。

其它说明：
    - 测速结果历史保存在数据目录 speedtest.db（boltdb 单文件），随应用升级保留。
    - 服务器名称可在应用中心「配置」里改（SERVER_NAME），改完停止→启动生效。
    - 每次测速完成后页面给出"测速编号"，历史页按编号可查单条，或看最近 100 条。
`, time.Now().Format("2006-01-02 15:04:05"), port, statsPassword)
	_ = os.WriteFile(txt, []byte(content), 0o644)
	log.Printf("已写凭据说明.txt")
}

// detectTimezone 读系统时区（沙箱 /etc/timezone 可读则用，否则北京时间）。
func detectTimezone() string {
	if tz := os.Getenv("TZ"); tz != "" {
		return tz
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	return "Asia/Shanghai"
}
