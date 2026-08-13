// SPDX-License-Identifier: AGPL-3.0-or-later
//
// UGOS Pro 原生应用适配层 —— 纯函数部分。
//
// 刻意【不加】 //go:build linux：这样它在开发机（macOS）上也参与编译，
// 配套的 ugos_port_test.go 才跑得起来。带 linux tag 的话 `go test` 会去
// 交叉编译一个 linux 测试二进制，在 macOS 上执行直接 exec format error。
// 函数在非 linux 下没有调用者，Go 不会为此报错。

package main

import "strings"

// portFromArgs 从 "--port=8080" / "--port 8080" 里取端口字符串，取不到返回空。
// 只做提取不做校验 —— 值不合法时上游 config.Load() 会忽略它并退回默认端口。
func portFromArgs(args []string) string {
	for i := 1; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--port="):
			return strings.TrimPrefix(args[i], "--port=")
		case args[i] == "--port" && i+1 < len(args):
			return args[i+1]
		}
	}
	return ""
}
