package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 「扫描不到音乐」是这类应用最常见的报障，而用户在绿联上没有 SSH、
// 上游的日志也只会说"找到 0 个音频文件"——到底是目录没授权、路径选错了层级、
// 还是里面真的没有支持的格式，从外面完全分不出来。
//
// 所以在 exec 之前自己把每个扫描目录看一眼，把结论直接写进日志。

// 上游 scanner 认的扩展名（README 和它的 file_walker 里列的那几种）。
var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".wav": true, ".m4a": true,
	".ogg": true, ".opus": true, ".ape": true, ".strm": true,
}

type rootDiag struct {
	Path    string
	Err     error // 目录本身打不开
	Audio   int   // 看到的音频文件数（数到 probeStopAt 就不数了）
	Dirs    int   // 子目录数，用来提示"是不是选高了一层"
	Partial bool  // 是不是提前停下的（大库/超时）
}

const (
	probeStopAt   = 50              // 数到这么多就够下结论了，别在几万首的库上空转
	probeMaxNodes = 20000           // 看过的条目上限
	probeTimeout  = 2 * time.Second // 冷存储/休眠的盘上 walk 会很慢，别拖住启动
)

// probeRoot 浅浅地看一眼这个目录，只为了能在日志里说人话。
func probeRoot(root string) rootDiag {
	d := rootDiag{Path: root}

	entries, err := os.ReadDir(root)
	if err != nil {
		d.Err = err
		return d
	}
	for _, e := range entries {
		if e.IsDir() {
			d.Dirs++
		}
	}

	deadline := time.Now().Add(probeTimeout)
	nodes := 0
	// filepath.WalkDir 的错误一律跳过：授权目录里混着读不了的子目录很正常，
	// 我们要的只是个量级。
	_ = filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		nodes++
		if nodes > probeMaxNodes || time.Now().After(deadline) {
			d.Partial = true
			return filepath.SkipAll
		}
		if e.IsDir() {
			// 上游扫描器跳过点开头的目录，我们也跳过，免得把 @eaDir 之类算进去
			if p != root && strings.HasPrefix(e.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if audioExts[strings.ToLower(filepath.Ext(e.Name()))] {
			d.Audio++
			if d.Audio >= probeStopAt {
				d.Partial = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return d
}

// diagnoseRoots 把每个扫描目录的实际情况翻译成一句用户看得懂的话。
// roots 为空时返回的是"根本没配目录"那条提示。
func diagnoseRoots(roots []string) []string {
	if len(roots) == 0 {
		return []string{
			"⚠ 当前一个扫描目录都没有，所以扫描一定是空的。两条路：" +
				"① 如果刚安装或升级过，去应用中心把本应用「停止」再「启动」一次 —— " +
				"平台是先起服务、两三秒后才写入安装参数的，首次启动读到的参数必然是空的；" +
				"② 或者打开应用，在「设置 - 音乐目录」里手动添加。",
		}
	}

	var out []string
	for _, r := range roots {
		d := probeRoot(r)
		switch {
		case d.Err == nil && d.Audio > 0:
			n := itoa(d.Audio)
			if d.Partial {
				n = n + "+"
			}
			out = append(out, "扫描目录 "+r+" 看到 "+n+" 个音频文件")
		case d.Err != nil && os.IsNotExist(d.Err):
			out = append(out, "⚠ 扫描目录 "+r+" 在沙箱里不存在。"+
				"如果刚安装或升级过，去应用中心「停止 → 启动」一次即可 —— "+
				"目录授权要重启后才会挂进来；否则请确认这个目录还在不在。")
		case d.Err != nil:
			out = append(out, "⚠ 扫描目录 "+r+" 打不开（"+d.Err.Error()+"）。"+
				"多半是没有授权给本应用：去应用中心的本应用设置里，把它加进「音乐文件夹」。")
		// 下面几条都是 Audio == 0。注意 probeRoot 是【递归】数的，
		// 所以不能说"可能歌在更深的层级里"—— 更深的层级已经看过了。
		case d.Partial:
			// 提前收手了（目录太大或太慢），没看到不代表没有，话要说软。
			out = append(out, "扫描目录 "+r+" 前若干层里还没看到音频文件"+
				"（目录很大或磁盘在唤醒，没有继续看下去）。如果扫描结果是空的，"+
				"多半是路径选错了。")
		case d.Dirs > 0:
			out = append(out, "⚠ 扫描目录 "+r+" 整个目录树里一个支持的音频文件都没有，"+
				"但下面有 "+itoa(d.Dirs)+" 个子文件夹 —— 多半是这个路径选错了层级。"+
				"支持的格式：mp3 / flac / wav / m4a / ogg / opus / ape / strm。")
		default:
			out = append(out, "⚠ 扫描目录 "+r+" 是空的，或者里面没有支持的格式"+
				"（mp3 / flac / wav / m4a / ogg / opus / ape / strm）。")
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
