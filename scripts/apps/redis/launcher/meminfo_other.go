//go:build !linux

package main

// 只为了让开发机（macOS）上的 go test / go build 能过。
// 真正跑起来的永远是 linux 那份，返回 0 时管理页会把"本机内存"那一行藏掉。
func totalMemoryMB() int64 { return 0 }
