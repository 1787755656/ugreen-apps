package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// 首启自愈：UGOS 平台是「先起服务、后写授权」——
// 服务起来约 1.6 秒后才写 init.d 的 .env，约 4 秒后才把 BindPaths= 写进 unit。
// 沙箱是挂载命名空间（TemporaryFileSystem=/:ro + PrivateMounts=yes），
// 用户选的目录【只有重启进程才会挂进来】。
//
// 所以全新安装的第一次启动，应用既拿不到参数、也看不到目录。应用自己把进程退出
// 一次，借 unit 里现成的 Restart=on-failure + RestartSec=5s 让 systemd 拉起来，
// 参数与挂载就都就位了 —— 用户不用去应用中心点「停止 → 启动」。
//
// 必须有次数上限，否则目录被删/改名后会变成重启循环。

const (
	healWindow    = 10 * time.Minute
	healMaxPerWin = 2
	healExitCode  = 3 // 非零 = failure → unit 的 Restart=on-failure 生效
)

type healState struct {
	Attempts []int64 `json:"attempts"` // unix 秒
	GaveUp   bool    `json:"gave_up,omitempty"`
	Note     string  `json:"note,omitempty"`
	path     string  `json:"-"`
}

func loadHealState(path string) *healState {
	st := &healState{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, st)
	}
	st.path = path
	return st
}

func (h *healState) save() {
	b, _ := json.MarshalIndent(h, "", " ")
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, h.path)
	}
}

// recent 返回窗口内的自愈次数（顺手丢掉过期记录）。
func (h *healState) recent() int {
	cut := time.Now().Add(-healWindow).Unix()
	kept := h.Attempts[:0]
	for _, ts := range h.Attempts {
		if ts >= cut {
			kept = append(kept, ts)
		}
	}
	h.Attempts = kept
	return len(kept)
}

func (h *healState) exhausted() bool {
	if h.GaveUp {
		return true
	}
	return h.recent() >= healMaxPerWin
}

func (h *healState) record() {
	h.Attempts = append(h.Attempts, time.Now().Unix())
	h.save()
}

func (h *healState) reset() {
	if len(h.Attempts) == 0 && !h.GaveUp {
		return
	}
	h.Attempts = nil
	h.GaveUp = false
	h.Note = ""
	h.save()
}

func (h *healState) giveUp(note string) {
	h.GaveUp = true
	h.Note = note
	h.save()
}

// startupRecord 落盘每次启动的现场。
// 首启那一刻的现场只存在一次（env 空、挂载缺失），不落盘事后就查不出来了。
type startupRecord struct {
	At          string   `json:"at"`
	EnvDir      string   `json:"env_download_dir"`
	UnitPaths   []string `json:"unit_bind_paths"`
	Download    string   `json:"download"`
	Source      string   `json:"download_source"`
	Active      bool     `json:"download_active"`
	Declared    string   `json:"declared,omitempty"`
	DeclaredErr string   `json:"declared_error,omitempty"`
	HealRecent  int      `json:"heal_attempts_recent"`
	HealGaveUp  bool     `json:"heal_gave_up,omitempty"`
	Uid         int      `json:"uid"`
}

// appendStartupRecord 保留最近 keep 条。
func appendStartupRecord(dataDir string, rec startupRecord, keep int) {
	p := filepath.Join(dataDir, "startup-log.json")
	var all []startupRecord
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	all = append(all, rec)
	if len(all) > keep {
		all = all[len(all)-keep:]
	}
	b, _ := json.MarshalIndent(all, "", " ")
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

// healDecision 在进程起来 8 秒后判定：平台已经声明授权目录、但沙箱里还用不了，
// 就主动退出一次。8 秒是给平台写 unit（~4 秒）留的余量，太早会误判成"没授权"。
func healDecision(p *Paths, st *healState) (shouldExit bool, reason string) {
	if p.DeclaredPath == "" || p.DownloadActive {
		return false, ""
	}
	// 万一在这 8 秒里平台补上了挂载（或目录被建出来了），就不用重启了
	if writableDir(p.DeclaredPath) {
		st.reset()
		return false, ""
	}
	if st.exhausted() {
		if !st.GaveUp {
			st.giveUp("自愈尝试已达上限，先以兜底目录运行")
			log.Printf("[自愈] 放弃：10 分钟内已尝试 %d 次，改用兜底目录 %s", len(st.Attempts), p.Fallback)
		}
		return false, ""
	}
	st.record()
	return true, "平台已授权目录 " + p.DeclaredPath + "，但沙箱里还没挂载/不可写"
}
