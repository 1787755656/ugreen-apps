//go:build linux

package main

// UGOS 沙箱没有 /usr/share/zoneinfo，嵌入 tzdata 保证时间显示正确（约 +450KB）。
// 其余沙箱适配（数据目录 / CA 证书 / TMPDIR）由 start.sh 通过环境变量完成。
import _ "time/tzdata"
