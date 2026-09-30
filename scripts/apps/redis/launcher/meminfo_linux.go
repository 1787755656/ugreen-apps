package main

import "syscall"

// totalMemoryMB 返回本机物理内存，用来在管理页上给"内存上限"一个参照。
//
// 【不能读 /proc/meminfo】：绿联原生沙箱里的 /proc 只有 self，那个文件根本不存在
// （2026-07-30 真机探针实测）。sysinfo(2) 是系统调用，不经过 /proc，照常可用。
//
// 单独一个文件是因为 syscall.Sysinfo 只有 Linux 有 —— 开发机是 macOS，
// 混在 main.go 里会让 `go test` 在本机直接编不过，那等于没有测试。
func totalMemoryMB() int64 {
	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err != nil {
		return 0
	}
	unit := uint64(si.Unit)
	if unit == 0 {
		unit = 1
	}
	return int64(uint64(si.Totalram) * unit / (1 << 20))
}
