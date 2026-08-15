package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePathList(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
	}{
		// 平台对 multi: true 的 path 参数给的就是 JSON 数组
		{"JSON 数组", `["/volume1/b","/volume1/a"]`, []string{"/volume1/a", "/volume1/b"}},
		// ⚠ 一个都没选时是字面量 null，不是空字符串
		{"一个都没选", "null", nil},
		// 首次启动时环境变量存在但值为空
		{"首启空值", "", nil},
		{"只有空白", "   ", nil},
		// multi 改回 false 时平台写裸标量，别整份丢掉
		{"裸路径", "/volume1/media", []string{"/volume1/media"}},
		{"去重和清理", `["/volume1/a/","/volume1/a","/volume1/b/../b"]`, []string{"/volume1/a", "/volume1/b"}},
		{"忽略相对路径", `["relative/path","/volume1/a"]`, []string{"/volume1/a"}},
		{"忽略空项", `["","/volume1/a"]`, []string{"/volume1/a"}},
		{"路径里有逗号和空格", `["/volume1/a, b/音乐"]`, []string{"/volume1/a, b/音乐"}},
		{"不是 JSON 也不是路径", "garbage", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parsePathList(c.raw)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("parsePathList(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

func TestReadParams(t *testing.T) {
	env := map[string]string{envMusicPaths: `["/volume1/music"]`}
	p := readParams(func(k string) string { return env[k] })
	if len(p.MusicPaths) != 1 || p.MusicPaths[0] != "/volume1/music" {
		t.Errorf("MusicPaths = %v", p.MusicPaths)
	}
}

// 管理员账号由上游自己的引导页创建，我们【不该】往子进程里塞任何凭据变量。
// 塞了的话上游会在"还没有账号"时按它建号，等于绕过引导页 —— 而首启参数是空的，
// 那个账号还注定要到第二次启动才建得出来，只会让人更糊涂。
func TestChildEnvHasNoCredentials(t *testing.T) {
	env := childEnv(testPaths(t))
	for _, k := range []string{"MUSIC_ADMIN_USER", "MUSIC_ADMIN_PASSWORD"} {
		if _, ok := hasEnvKey(env, k); ok {
			t.Errorf("不该出现 %s", k)
		}
	}
}

// 首次启动读到的是全空 —— 这条路必须干净走通，不能报错。
func TestReadParamsFirstBoot(t *testing.T) {
	p := readParams(func(string) string { return "" })
	if len(p.MusicPaths) != 0 {
		t.Errorf("首启应当全空，实际 %+v", p)
	}
}

func TestChildEnv(t *testing.T) {
	p := testPaths(t)
	env := childEnv(p)

	// PATH 里包内 bin 必须排最前 —— 上游按 PATH 找 ffmpeg，
	// 而沙箱里 /usr/bin 和 /bin 字面不存在。
	path, ok := hasEnvKey(env, "PATH")
	if !ok || !strings.HasPrefix(path, p.Bin+":") {
		t.Errorf("PATH 没把包内 bin 排最前：%q", path)
	}
	if v, _ := hasEnvKey(env, "LD_LIBRARY_PATH"); v != p.Lib {
		t.Errorf("LD_LIBRARY_PATH = %q，libtag 找不到的话上游根本起不来", v)
	}
	// 沙箱里没有 /tmp，也没有 /etc/passwd
	if v, _ := hasEnvKey(env, "TMPDIR"); !strings.HasPrefix(v, p.Cache) {
		t.Errorf("TMPDIR 应当落在缓存目录里，实际 %q", v)
	}
	if v, _ := hasEnvKey(env, "HOME"); v == "" || v == "/" {
		t.Errorf("HOME = %q", v)
	}
	if v, _ := hasEnvKey(env, "MUSIC_KEY_PEPPER_FILE"); filepath.Dir(v) == "" {
		t.Errorf("MUSIC_KEY_PEPPER_FILE = %q", v)
	}
}
