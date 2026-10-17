package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 进度解析：N_m3u8DL-RE 用 Spectre.Console 画多条进度条，同一帧里
// 视频/音频/字幕各占一行、之间【没有换行】（实测是 \r 覆盖 + 定宽填充，
// 落到文件里就变成一整串），真实片段形如
//
//	Vid 1920x1080 | 7968 Kbps | 60 ━━━━━ 75/101 74.26% 426.09MB/573.80MB39.40MBps00:00:03
//	Aud English | en | 6CH        ━━━━━ 102/102 100.00%27.48MB   -     00:00:00
//
// 所以不能"取最后一次匹配"，否则拿到的是跑得最慢的那条流（通常是字幕）。
// 这里按「分片/百分比」三元组切出每一帧，取进度最大的那一帧做代表，
// 再从该帧之后的一小段窗口里取体积/速度/剩余时间。
var (
	reSegPct   = regexp.MustCompile(`([0-9]+)/([0-9]+)[ \t]+([0-9]{1,3}(?:\.[0-9]+)?)%`)
	reSpeed    = regexp.MustCompile(`[0-9.]+ ?[KMGT]?Bps`)
	reDlTotal  = regexp.MustCompile(`[0-9.]+ ?[KMGT]?B/[0-9.]+ ?[KMGT]?B`)
	reETA      = regexp.MustCompile(`[0-9]{2}:[0-9]{2}:[0-9]{2}`)
	reStream   = regexp.MustCompile(`\b(Vid|Aud|Sub)\b`)
	reANSI     = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	reBarOnly  = regexp.MustCompile(`^\[[#. =\-]*\]$`)
	reSpeedN   = regexp.MustCompile(`^([0-9.]*) *([KMGT])Bps$`)
	reLogStamp = regexp.MustCompile(`[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+`)
)

type progress struct {
	Percent    float64
	Speed      string
	Segments   string
	Downloaded string
	Total      string
	ETA        string
	Stream     string
	Last       string
	Stage      string
}

func normalizeSpeed(s string) string {
	if s == "" {
		return ""
	}
	if m := reSpeedN.FindStringSubmatch(s); m != nil {
		if m[2] == "" {
			return m[1] + " B/s"
		}
		return m[1] + " " + m[2] + "B/s"
	}
	return s
}

func trimRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func window(s string, from, n int) string {
	if from < 0 || from >= len(s) {
		return ""
	}
	end := from + n
	if end > len(s) {
		end = len(s)
	}
	return s[from:end]
}

func lastStream(s string) string {
	m := reStream.FindAllString(s, -1)
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}

// stageOf 判断当前阶段。
//
// ⚠ 不能只按"最后出现的某个关键词"判：真机日志里 N_m3u8DL-RE 是**按流分段**
// 下载的（先视频、再音频、再字幕），每段之间都会插一条
// `INFO : Binary merging...`，紧接着又是一条 `INFO : Start downloading...`，
// 于是 合并/下载 两个关键字会来回跳，卡片上的阶段就跟着抖。
// 正确的近似是：没到 100% 一律算"下载中"；到 100% 之后才区分混流/合并。
func stageOf(raw string, pct float64, hasFrame bool) string {
	idx := func(words ...string) int {
		best := -1
		for _, w := range words {
			if i := strings.LastIndex(raw, w); i > best {
				best = i
			}
		}
		return best
	}

	if !hasFrame {
		if idx("解析", "arsing", "探测", "robe") >= 0 {
			return "解析中"
		}
		return "处理中"
	}
	if pct < 99.5 {
		return "下载中"
	}
	mux := idx("混流", "Mux", "mux", "MUX")
	merge := idx("合并", "merg", "Merg", "Merging")
	switch {
	case mux >= 0 && mux > merge:
		return "混流中"
	case merge >= 0:
		return "合并中"
	default:
		return "下载中"
	}
}

// parseProgress 输入日志尾部原文（最多 128KB），输出归一化进度。
func parseProgress(raw string) progress {
	var pr progress
	if raw == "" {
		return pr
	}
	raw = reANSI.ReplaceAllString(raw, "")
	// Spectre 的日志时间戳形如 21:38:32.105，会被 ETA 正则误吞 → 先抹掉
	raw = reLogStamp.ReplaceAllString(raw, " ")
	raw = strings.ToValidUTF8(raw, "")
	// \r 与 \n 都当行分隔，进度帧才不会被当成一整行
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")

	pr.Percent = -1
	locs := reSegPct.FindAllStringSubmatchIndex(raw, -1)
	for _, l := range locs {
		pct, _ := strconv.ParseFloat(raw[l[6]:l[7]], 64)
		if pct <= pr.Percent {
			continue
		}
		pr.Percent = pct
		pr.Segments = raw[l[2]:l[3]] + "/" + raw[l[4]:l[5]]
		w := window(raw, l[1], 160)
		if m := reDlTotal.FindString(w); m != "" {
			if i := strings.IndexByte(m, '/'); i > 0 {
				pr.Downloaded = strings.TrimSpace(m[:i])
				pr.Total = strings.TrimSpace(m[i+1:])
			}
		}
		pr.Speed = normalizeSpeed(reSpeed.FindString(w))
		pr.ETA = reETA.FindString(w)
		// 流类型：往这一帧前面找最近的 Vid/Aud/Sub 标签
		// 进度条本体就有 30 个 ━（90 字节），120 字节的回看窗口会落在条中间
		start := l[0] - 240
		if start < 0 {
			start = 0
		}
		pr.Stream = lastStream(raw[start:l[0]])
	}
	if pr.Percent < 0 {
		pr.Percent = 0
	}
	if pr.Percent > 100 {
		pr.Percent = 100
	}

	// 最新一行（跳过空行与纯进度条碎片）
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if ln == "" || reBarOnly.MatchString(ln) {
			continue
		}
		pr.Last = trimRunes(ln, 300)
		break
	}

	pr.Stage = stageOf(raw, pr.Percent, len(locs) > 0)
	return pr
}

// readLogTail 读取日志尾部最多 n 字节。截断点可能落在 UTF-8 字符中间，
// 这里把开头的残字节丢掉（进度帧之间没有换行，不能靠找 \n 对齐）。
func readLogTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	size := st.Size()
	off := int64(0)
	if size > n {
		off = size - n
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && len(buf) == 0 {
		return ""
	}
	for len(buf) > 0 && !utf8.RuneStart(buf[0]) {
		buf = buf[1:]
	}
	return string(buf)
}
