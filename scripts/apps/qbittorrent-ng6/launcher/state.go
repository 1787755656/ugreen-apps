package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// state 记录"上一次已经应用过的参数值"。
//
// 为什么不是每次启动都照参数刷一遍配置：那些设置在 WebUI 里也能改，
// 每次启动照参数覆盖会把用户在 WebUI 里的修改冲掉。
// 只在**参数本身发生变化**时才写进配置 —— 用户改设置有效，WebUI 改的也保得住。
type state struct {
	// Initialized 只在**首次初始化整体成功后**才置 true。
	// 别用"配置目录非空"这种弱判据：某一步失败时目录已经非空，
	// 之后每次启动都会认为"已初始化"而跳过，服务永久停在半成品状态且不报错。
	Initialized bool `json:"initialized"`

	AppliedDownloadPath string `json:"applied_download_path"`
	AppliedBTPort       int    `json:"applied_bt_port"`
}

func statePath(dataDir string) string { return filepath.Join(dataDir, "ugos-state.json") }

func loadState(dataDir string) state {
	var st state
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return st
	}
	// 文件坏了就当没有：大不了重新按参数应用一次，比启动失败强。
	_ = json.Unmarshal(data, &st)
	return st
}

func saveState(dataDir string, st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(statePath(dataDir), append(data, '\n'), 0o600)
}

// writeFileAtomic 先写同目录的临时文件再 rename，避免掉电留下半个文件。
// 临时文件必须建在**目标目录**里：沙箱没有 /tmp，而且跨设备 rename 会失败。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
