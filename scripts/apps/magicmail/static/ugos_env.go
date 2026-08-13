// SPDX-License-Identifier: AGPL-3.0-or-later
//
// UGOS Pro 原生应用适配层 —— 由 magicmail-ugreen-app 在构建时拷贝进上游的
// server/ 目录一起编译，上游仓库本身保持原样（不打 patch、不 fork）。
//
// 只做一件事：把绿联沙箱注入的环境变量对到 Magicmail 自己认识的环境变量上。
// 不在沙箱里跑（UGAPP_DATA_DIR 为空）时整个文件不产生任何行为，本地开发和
// Docker 部署完全不受影响。

// ugossmoke 这个 tag 只为了在开发机（macOS）上做冒烟测试时能把本文件编进去，
// 见 scripts/smoke.sh。正式产物永远走 linux 这一支，不会带这个 tag。
//go:build linux || ugossmoke

package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

func init() {
	dataDir := strings.TrimSpace(os.Getenv("UGAPP_DATA_DIR"))
	if dataDir == "" {
		return // 不在绿联沙箱里，保持上游默认行为
	}

	// --- 监听端口 ---------------------------------------------------------
	// 上游只认 MAGICMAIL_PORT 环境变量，而 project.yaml 的 start_cmd 里没法设
	// 环境变量（不经 shell）。所以这里从命令行参数取 --port=N，让 project.yaml
	// 的 start_cmd 成为端口的唯一来源，避免"二进制里写死的端口"和"project.yaml
	// 里声明的 port"两处不一致——那会让应用中心一直显示"未启动"。
	// 上游 main() 不解析任何参数，多传一个不会冲突。
	if p := portFromArgs(os.Args); p != "" {
		os.Setenv("MAGICMAIL_PORT", p)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Printf("[ugos] 数据目录不可用 %s: %v", dataDir, err)
		return
	}

	// --- 工作目录 ---------------------------------------------------------
	// 附件落盘走的是相对路径 ./data/attachments，硬编码在三处
	// （services/attachment_service.go、imap/fetcher.go、pop3/fetcher.go），
	// 且没有环境变量可以覆盖。安装目录在沙箱里是只读的，所以必须把工作目录
	// 切到数据目录，否则第一封带附件的邮件就会写失败。
	if err := os.Chdir(dataDir); err != nil {
		log.Printf("[ugos] 切换工作目录失败 %s: %v", dataDir, err)
	}

	// --- 数据库 -----------------------------------------------------------
	// 上游默认 "data/magicmail.db"（相对工作目录）。显式指到数据目录根下，
	// 迁移安装目录时跟着 UGAPP_DATA_DIR 走（project.yaml 里 support_migration: true）。
	if v := strings.TrimSpace(os.Getenv("MAGICMAIL_DSN")); v == "" {
		os.Setenv("MAGICMAIL_DSN", filepath.Join(dataDir, "magicmail.db"))
	}

	// --- 临时目录 ---------------------------------------------------------
	// 绿联的原生沙箱里【没有 /tmp】，而 os.TempDir() 照旧返回 "/tmp"。
	// 上游当前没有直接用 os.TempDir()，但依赖库（如 multipart 表单落盘）会用，
	// 所以先把 TMPDIR 指到缓存目录，免得以后升级上游时踩这个坑。
	if v := strings.TrimSpace(os.Getenv("TMPDIR")); v == "" {
		cacheDir := strings.TrimSpace(os.Getenv("UGAPP_CACHE_DIR"))
		if cacheDir == "" {
			cacheDir = filepath.Join(dataDir, "tmp")
		}
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			os.Setenv("TMPDIR", cacheDir)
		} else {
			log.Printf("[ugos] 临时目录不可用 %s: %v", cacheDir, err)
		}
	}

	log.Printf("[ugos] 绿联沙箱适配完成: data=%s dsn=%s port=%s tmp=%s",
		dataDir, os.Getenv("MAGICMAIL_DSN"), os.Getenv("MAGICMAIL_PORT"), os.Getenv("TMPDIR"))
}
