//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// UGOS Pro 沙箱适配。
//
// 打包时把本文件拷进上游的 cmd/litepan/ 一起编译，上游仓库保持原样。
// init() 在 main() 里 config.Load() 之前执行，所以这里设的环境变量
// 就是 LitePan 自己认的那套 LITEPAN_* 覆盖项，不需要改上游任何代码。
//
// 三件事：
//  1. 数据目录指向平台注入的 UGAPP_DATA_DIR（应用迁移安装目录后自动跟上）。
//  2. STRM 输出目录兜底 —— 见下方注释，留空时上游默认值在沙箱里不可写。
//  3. TMPDIR 指到应用自己的缓存目录 —— 沙箱里【没有 /tmp】，
//     os.TempDir() 会返回一个不存在的路径。
func init() {
	dataDir := strings.TrimSpace(os.Getenv("UGAPP_DATA_DIR"))
	if dataDir == "" {
		// 不在 UGOS 沙箱里（本地 go run / docker），保持上游默认行为。
		return
	}

	if strings.TrimSpace(os.Getenv("LITEPAN_DATA_DIR")) == "" {
		_ = os.Setenv("LITEPAN_DATA_DIR", dataDir)
	}

	// STRM 目录来自 project.yaml 的 path 参数。没选时上游的
	// config.StrmDirForData() 会算成数据目录的【上一级】，也就是
	// /volume1/@appdata —— 那一层在沙箱里不可写，必须自己兜底。
	//
	// 另外全新安装/升级后的第一次启动，平台还没写入参数值和授权目录
	// （要重启一次才生效），所以这里拿到空值是正常的，同样走兜底。
	strmDir := strings.TrimSpace(os.Getenv("LITEPAN_STRM_DIR"))
	fallbackStrm := filepath.Join(dataDir, "strm")
	if strmDir == "" || strmDir == "null" {
		strmDir = fallbackStrm
	}
	if err := os.MkdirAll(strmDir, 0o755); err != nil && strmDir != fallbackStrm {
		// 选中的目录还没被 bind 进沙箱（首次启动），退回数据目录，
		// 保证应用能起来；用户重启一次后就会用上真正选的目录。
		strmDir = fallbackStrm
		_ = os.MkdirAll(strmDir, 0o755)
	}
	_ = os.Setenv("LITEPAN_STRM_DIR", strmDir)

	tmpDir := strings.TrimSpace(os.Getenv("UGAPP_CACHE_DIR"))
	if tmpDir == "" {
		tmpDir = filepath.Join(dataDir, "tmp")
	}
	if err := os.MkdirAll(tmpDir, 0o755); err == nil {
		_ = os.Setenv("TMPDIR", tmpDir)
	}
}
