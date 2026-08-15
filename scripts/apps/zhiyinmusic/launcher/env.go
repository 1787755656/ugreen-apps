package main

import (
	"os"
	"strings"
)

// childEnv 组装交给上游二进制的环境。
//
// 上游几乎所有配置都走 config.toml（见 config.go），环境变量只有四个 ——
// 但这里给的每一条都对应一个真实的坑，改动前先看注释。
//
// 用 syscall.Exec 换掉自己时环境是【整份替换】的，所以该带的必须显式带上。
func childEnv(p Paths) []string {
	env := []string{
		// 上游查 ffmpeg 的顺序是 PATH → /usr/bin/ffmpeg → /usr/local/bin/ffmpeg，
		// 而沙箱里【/usr/bin 和 /bin 字面不存在】（没声明 EXEC 权限时不会被
		// bind 进来，而且那份白名单里本来也没有 ffmpeg）。包内 bin 必须排最前。
		// 少了它的症状是"转码不可用"和 strm 探测拿不到时长，日志里只有一句
		// ffmpeg not found in system PATH。
		"PATH=" + p.Bin + ":/usr/local/bin:/usr/bin:/bin",

		// 上游是动态链接的，libtag / libtag_c 在镜像里装在 /usr/lib 下，
		// 而沙箱里没有 /usr/lib。随包带一份，靠这个变量找到。
		"LD_LIBRARY_PATH=" + p.Lib,

		// 沙箱里【没有 /tmp】。Rust 的 std::env::temp_dir() 照样返回 /tmp，
		// 于是封面写盘、刮削下载这类"先写临时文件再改名"的操作会直接失败。
		"TMPDIR=" + p.TmpDir(),

		// 沙箱里没有 /etc/passwd，求 home 目录的代码会失败。
		"HOME=" + p.Home(),

		// Subsonic 密码加密用的 pepper。不指定的话上游会写到
		// <数据库目录>/.zhiyin/key_pepper —— 那个位置其实也可写，
		// 但显式钉住更省心：换了数据目录也不会让老客户端突然认不了密码。
		"MUSIC_KEY_PEPPER_FILE=" + p.PepperFile(),

		// 和上游镜像保持一致。日志走 unit 的 StandardOutput=append，
		// 落在 /volume1/@appstore/<appid>/log/<appid>.log。
		"RUST_LOG=" + envOr("RUST_LOG", "info"),
		"RUST_BACKTRACE=1",
	}

	// 时区：平台会注入 TZ；没有就跟随沙箱里的 /etc/localtime（那个是在的）。
	for _, k := range []string{"TZ", "LANG", "LC_ALL"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// hasEnvKey 只给测试用：确认某个变量确实出现在组装结果里。
func hasEnvKey(env []string, key string) (string, bool) {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"="), true
		}
	}
	return "", false
}
