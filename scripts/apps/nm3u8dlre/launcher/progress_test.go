package main

import (
	"math"
	"os"
	"strings"
	"testing"
)

// 以下片段全部来自真机（绿联 arm64 NAS 沙箱内跑 Apple bipbop 示例流）的日志，
// 不是手写样例 —— 上游的坑都在这些"看起来不像进度帧"的字节里。
const frameMid = "22:38:33.100 INFO : Start downloading..." +
	"Vid 1920x1080 | 7968 Kbps | 60 \u2501\u2501\u2501\u2501 84/101 83.17% 467.98MB/562.69MB41.89MBps00:00:02" +
	"Aud English | en | 6CH         \u2501\u2501\u2501\u2501   0/100 0.00%                 -     -     --:--:--" +
	"Sub en | English               \u2501\u2501\u2501\u2501   0/100 0.00%                 -     -     --:--:--"

const frameDone = "21:39:09.724 WARN : Muxing to sandboxtest.MUX.mp4" +
	"Vid 1920x1080 | 7968 Kbps | 60 \u2501\u2501\u2501\u2501 101/101 100.00%558.38MB    -     00:00:00" +
	"Aud English | en | 6CH         \u2501\u2501\u2501\u2501 102/102 100.00%27.48MB    -     00:00:00" +
	"Sub en | English               \u2501\u2501\u2501\u2501  100/100 100.00%35.34KB    -     00:00:00"

func TestParseProgressMidDownload(t *testing.T) {
	p := parseProgress(frameMid)
	// 关键回归：后面两条流是 0.00%，不能盖掉视频的 83.17%
	if math.Abs(p.Percent-83.17) > 0.01 {
		t.Fatalf("Percent = %v, 期望 83.17", p.Percent)
	}
	if p.Segments != "84/101" {
		t.Fatalf("Segments = %q, 期望 84/101", p.Segments)
	}
	if p.Downloaded != "467.98MB" || p.Total != "562.69MB" {
		t.Fatalf("体积 = %q/%q, 期望 467.98MB/562.69MB", p.Downloaded, p.Total)
	}
	if p.Speed != "41.89 MB/s" {
		t.Fatalf("Speed = %q, 期望 41.89 MB/s", p.Speed)
	}
	if p.ETA != "00:00:02" {
		t.Fatalf("ETA = %q, 期望 00:00:02", p.ETA)
	}
	if p.Stream != "Vid" {
		t.Fatalf("Stream = %q, 期望 Vid", p.Stream)
	}
	if p.Stage != "下载中" {
		t.Fatalf("Stage = %q, 期望 下载中", p.Stage)
	}
}

func TestParseProgressDone(t *testing.T) {
	p := parseProgress(frameDone)
	if p.Percent != 100 {
		t.Fatalf("Percent = %v, 期望 100", p.Percent)
	}
	if p.Segments != "101/101" && p.Segments != "102/102" && p.Segments != "100/100" {
		t.Fatalf("Segments = %q，三条流之一才对", p.Segments)
	}
	// 混流的任务在结束时最后一条事件是 Muxing，阶段应落到混流而不是解析
	if p.Stage != "混流中" {
		t.Fatalf("Stage = %q, 期望 混流中", p.Stage)
	}
}

// 日志行首的时间戳（21:39:09.724）绝不能被当成剩余时间。
func TestParseProgressTimestampIsNotETA(t *testing.T) {
	p := parseProgress("21:39:09.724 INFO : Done")
	if p.ETA != "" {
		t.Fatalf("ETA = %q，不应把日志时间戳当剩余时间", p.ETA)
	}
}

// 早期日志里有 "MuxAfterDone is detected"，不能因此一直显示"混流中"。
func TestParseProgressEarlyMuxIsNotStage(t *testing.T) {
	raw := "21:38:32.106 WARN : MuxAfterDone is detected, binary merging is enabled\n" +
		"21:38:33.100 INFO : Start downloading...\n" +
		frameMid
	if p := parseProgress(raw); p.Stage != "下载中" {
		t.Fatalf("Stage = %q, 期望 下载中（MuxAfterDone 是早期事件）", p.Stage)
	}
}

func TestParseProgressEmpty(t *testing.T) {
	if p := parseProgress(""); p.Percent != 0 || p.Stage != "" {
		t.Fatalf("空输入应返回零值，得到 %+v", p)
	}
}

// 截断点落在多字节字符中间时不能产生 U+FFFD。
func TestReadLogTailTruncation(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.log"
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 10)+"\u2501\u2501\u2501"+"tail"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 只读最后 6 字节，会切在 ━ 的中间
	got := readLogTail(path, 6)
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("tail 含替换字符: %q", got)
	}
	if !strings.HasSuffix(got, "tail") {
		t.Fatalf("tail = %q", got)
	}
}

// 真机日志里引擎是「按流分段」下载的：每段之间插一条 Binary merging...，
// 紧接着又一条 Start downloading...。阶段不能跟着这两条来回跳，
// 未到 100% 一律显示下载中。
func TestStageDoesNotFlickerBetweenStreams(t *testing.T) {
	raw := "21:42:19.823 WARN : fMP4 is detected, binary merging is automatically enabled\n" +
		"21:42:19.823 INFO : Start downloading...\n" +
		"Vid 1920x1080 | 7968 Kbps | 60 \u2501\u2501 50/101 49.50% 280.10MB/565.80MB39.10MBps00:00:08\n" +
		"21:42:33.804 INFO : Binary merging...\n" +
		"Vid 1920x1080 | 7968 Kbps | 60 \u2501\u2501 60/101 59.41% 336.00MB/565.80MB38.00MBps00:00:06\n"
	p := parseProgress(raw)
	if p.Stage != "下载中" {
		t.Fatalf("Stage = %q, 期望 下载中（59%% 时不该显示合并/混流）", p.Stage)
	}
	if p.Stream != "Vid" {
		t.Fatalf("Stream = %q, 期望 Vid", p.Stream)
	}
}

// 进度条本体有 30 个 ━（90 字节），Vid 标签离数字超过 120 字节，
// 回看窗口太窄会取不到流类型。
func TestStreamLabelSurvivesLongBar(t *testing.T) {
	bar := strings.Repeat("\u2501", 30)
	raw := "Vid 1920x1080 | 7968 Kbps | 60 " + bar +
		" 4242/4242 42.42% 424.20MB/999.90MB42.00MBps00:00:42"
	p := parseProgress(raw)
	if p.Stream != "Vid" {
		t.Fatalf("Stream = %q（长进度条后仍应识别出 Vid）", p.Stream)
	}
	if p.Percent != 42.42 {
		t.Fatalf("Percent = %v, 期望 42.42", p.Percent)
	}
}
