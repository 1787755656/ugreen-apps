package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// qBittorrent 的 IP 地理数据库（用来显示 peer 的国家/地区）。
//
// 它自己会在启动后去 db-ip.com 下载，但**首次启动那一刻库还不存在**，
// 执行日志里就会先来一条吓人的
//     无法加载 IP 地理数据库。原因：No such file or directory
// 而且这条自愈依赖能连上 db-ip.com —— 国内网络下不一定通，连不上就一直是这个状态。
//
// 所以包里自带一份，首次启动直接铺过去：一上来就是"IP 地理数据库已加载"。
// 之后 qBittorrent 自己的月度更新照常工作，会把它换成更新的版本。
//
// 数据来源 DB-IP（https://db-ip.com），CC BY 4.0，随包附了署名文件。
const (
	geoipDirName  = "GeoDB"
	geoipFileName = "dbip-country-lite.mmdb"
	// 相对安装目录（bin/ 的上一级）的位置，对应 rootfs_common/geoip/
	geoipBundledRel = "geoip/" + geoipFileName + ".gz"
	// 解压出来明显小于这个数就说明包里的东西不对，别把半个库铺过去
	geoipMinSize = 1 << 20
)

// ensureGeoIPDatabase 只在目标不存在时铺一次。
// 任何失败都只记日志：地理库没有也就是看不到 peer 的国旗，不该影响下载。
func ensureGeoIPDatabase(l layout, installDir string) {
	target := filepath.Join(l.profileDir, "qBittorrent", "data", geoipDirName, geoipFileName)
	if st, err := os.Stat(target); err == nil && st.Size() > 0 {
		return // 已经有了（自带的或 qBittorrent 自己更新下来的），不要碰
	}

	src := filepath.Join(installDir, geoipBundledRel)
	n, err := installGzip(src, target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logf("包内没有自带 IP 地理数据库，等 qBittorrent 自己联网下载")
			return
		}
		logf("警告：安装自带的 IP 地理数据库失败：%v（不影响下载功能）", err)
		return
	}
	logf("已安装自带的 IP 地理数据库（%.1f MB）", float64(n)/(1<<20))
}

func installGzip(src, target string) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("解压 %s: %w", src, err)
	}
	defer zr.Close()

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	// 先解到同目录的临时文件再改名：解到一半掉电也不会留下一个
	// 看起来像数据库、其实是半截的文件（qBittorrent 会加载失败）。
	tmp, err := os.CreateTemp(filepath.Dir(target), ".geoip-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	n, err := io.Copy(tmp, zr)
	if err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if n < geoipMinSize {
		return 0, fmt.Errorf("解压出来只有 %d 字节，包内的库不完整", n)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp.Name(), target)
}
