package main

// 从 UGAPP_SHARED_DIR 发现真实 bind 进沙箱的授权目录。
//
// 平台把用户授权的目录按【完整镜像卷路径】挂在 shared 下（真机实测的三层结构）：
//
//	$UGAPP_SHARED_DIR/volume1/<共享文件夹>/<授权目录>     ← 只有叶子是真实挂载
//
// 中间层是合成的只读视图。发现逻辑：从 shared 根往下走，【每一步都真的
// ReadDir】—— 打不开的分支直接放弃；走到的每一个"真实挂载"都记下来。
//
// 怎么区分"真实挂载"和"合成父目录"：看 /proc/self/mountinfo。
// 沙箱是挂载命名空间隔离的，BindPaths 挂载的每个授权目录在里面都有
// 一条记录（真机实测）。这条信息比"猜哪层是叶子"可靠得多。
//
// 兜底：读不到 mountinfo 时（理论不会发生），把 shared 下能打开的
// 目录全部记下来 —— 多记无害，上游 Guard 会用真实打开再筛一遍，
// 沙箱本身的可见性就是最终防线。

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

func sharedRoot() string {
	return strings.TrimSpace(os.Getenv("UGAPP_SHARED_DIR"))
}

// discoverSharedMounts 返回 shared 下所有真实挂载的目录，
// 已换算回真实路径形式（/volume1/...）。
func discoverSharedMounts() []string {
	root := sharedRoot()
	if root == "" {
		return nil
	}

	binds := bindPathsFromMountinfo()
	if len(binds) > 0 {
		var out []string
		for _, p := range binds {
			// 只要用户卷上的目录：/volume1/... 形式。
			// 应用自身的 data/cache/log 挂载对上游没意义，过滤掉。
			if strings.HasPrefix(p, "/volume1/") || strings.HasPrefix(p, "/volume2/") ||
				strings.HasPrefix(p, "/volume3/") || strings.HasPrefix(p, "/volume4/") {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
		// mountinfo 里有记录但没有 /volumeN 条目 → 确实没授权用户目录
		return nil
	}

	// 兜底：递归走 shared 树（没有 mountinfo 时才走这条路）。
	// 打不开的分支直接放弃；能打开的目录都记下来。
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > 6 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			child := filepath.Join(dir, e.Name())
			if _, err := os.ReadDir(child); err != nil {
				continue // 打不开 = 不是我们的
			}
			out = append(out, sharedPathToReal(child))
			walk(child, depth+1)
		}
	}
	walk(root, 0)
	return out
}

// bindPathsFromMountInfo 从 /proc/self/mountinfo 里找出所有"bind 到真实路径"的挂载点。
//
// 沙箱根是 tmpfs（TemporaryFileSystem=/:ro），真实目录都以 bind 挂载出现，
// 挂载点路径就是真实路径本身（如 /volume1/test/testapp）。
func bindPathsFromMountinfo() []string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// mountinfo 格式：36 35 0:30 / /volume1/test/testapp rw,relatime - btrfs ...
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountPath(fields[4])
		if mountPoint == "/" {
			continue
		}
		// 排除平台自己的伪挂载和 tmpfs 根
		switch {
		case strings.HasPrefix(mountPoint, "/proc"),
			strings.HasPrefix(mountPoint, "/sys"),
			strings.HasPrefix(mountPoint, "/dev"),
			strings.HasPrefix(mountPoint, "/run"),
			mountPoint == "/lib", mountPoint == "/lib64", mountPoint == "/lib32",
			mountPoint == "/bin", mountPoint == "/sbin",
			mountPoint == "/usr/bin", mountPoint == "/usr/sbin",
			mountPoint == "/etc/localtime", mountPoint == "/etc/resolv.conf",
			mountPoint == "/etc/ssl", mountPoint == "/etc/timezone",
			strings.HasPrefix(mountPoint, "/var/packages/"):
			continue
		}
		if !seen[mountPoint] {
			seen[mountPoint] = true
			out = append(out, mountPoint)
		}
	}
	return out
}

// unescapeMountPath 还原 mountinfo 第 5 列的转义（\040 空格 \011 制表 \134 反斜杠 \012 换行）。
func unescapeMountPath(p string) string {
	if !strings.Contains(p, "\\") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' && i+3 < len(p) {
			switch p[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 3
				continue
			case "011":
				b.WriteByte('\t')
				i += 3
				continue
			case "134":
				b.WriteByte('\\')
				i += 3
				continue
			case "012":
				b.WriteByte('\n')
				i += 3
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// sharedPathToReal 把 shared 镜像路径换算回真实路径：
// <shared>/volume1/test/testapp → /volume1/test/testapp。
func sharedPathToReal(p string) string {
	root := sharedRoot()
	if root != "" && strings.HasPrefix(p, root+"/") {
		return "/" + strings.TrimPrefix(p, root+"/")
	}
	return p
}
