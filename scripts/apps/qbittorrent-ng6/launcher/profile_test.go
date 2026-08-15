package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLayout(t *testing.T) layout {
	t.Helper()
	base := t.TempDir()
	l := newLayout(filepath.Join(base, "data"), filepath.Join(base, "cache"))
	if err := os.MkdirAll(l.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureProfile(l); err != nil {
		t.Fatal(err)
	}
	return l
}

func readConf(t *testing.T, l layout) *iniFile {
	t.Helper()
	raw, err := os.ReadFile(l.confPath)
	if err != nil {
		t.Fatal(err)
	}
	return parseINI(raw)
}

func TestEnsureProfileIsIdempotent(t *testing.T) {
	l := newTestLayout(t)
	// 用户改过的插件不该被下一次启动覆盖回去。
	victim := filepath.Join(l.enginesDir, "piratebay.py")
	if err := os.WriteFile(victim, []byte("# 用户改过\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureProfile(l); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil || !strings.Contains(string(got), "用户改过") {
		t.Fatalf("用户修改被覆盖了：%q %v", got, err)
	}

	entries, err := os.ReadDir(l.enginesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 30 {
		t.Fatalf("搜索插件没装全，只有 %d 个", len(entries))
	}
}

func TestApplyParamsFirstBootFallsBackToDataDir(t *testing.T) {
	l := newTestLayout(t)
	// 首启：参数全空（平台还没写 .env），授权目录也还没挂进来。
	st, err := applyParams(l, loadParams(lookupFrom(nil)), state{})
	if err != nil {
		t.Fatal(err)
	}
	if st.AppliedDownloadPath != l.fallbackDownloadDir() {
		t.Fatalf("应回落到数据目录，得到 %q", st.AppliedDownloadPath)
	}
	if _, err := os.Stat(l.fallbackDownloadDir()); err != nil {
		t.Fatalf("兜底目录没建出来：%v", err)
	}

	conf := readConf(t, l)
	if v, _ := conf.GetPref(keySavePath); v != l.fallbackDownloadDir()+"/" {
		t.Fatalf("保存路径没写对：%q", v)
	}
	// 首启没有密码参数 → 必须落一个已知密码，否则用户根本进不去 WebUI。
	if _, ok := conf.Get("Preferences", `WebUI\Password_PBKDF2`); !ok {
		t.Fatal("首次初始化必须写入默认密码")
	}
	// 端口对局域网直接可达，防护要开着（上游镜像为了套反代把它们关了）。
	for _, k := range []string{`WebUI\CSRFProtection`, `WebUI\ClickjackingProtection`} {
		if v, _ := conf.Get("Preferences", k); v != "true" {
			t.Fatalf("%s 应为 true，得到 %q", k, v)
		}
	}
}

// 第二次启动（用户重启过、平台已经写好 .env 并挂上授权目录）参数才真正到位。
func TestApplyParamsSecondBootPicksUpRealPath(t *testing.T) {
	l := newTestLayout(t)
	st, _ := applyParams(l, loadParams(lookupFrom(nil)), state{})
	st.Initialized = true

	real := filepath.Join(t.TempDir(), "媒体库", "下载")
	p := loadParams(lookupFrom(map[string]string{envDownloadPath: real, envBTPort: "27000"}))
	st, err := applyParams(l, p, st)
	if err != nil {
		t.Fatal(err)
	}
	if st.AppliedDownloadPath != real {
		t.Fatalf("没跟上新的下载目录：%q", st.AppliedDownloadPath)
	}
	conf := readConf(t, l)
	if v, _ := conf.GetPref(keySavePath); v != real+"/" {
		t.Fatalf("保存路径没更新：%q", v)
	}
	if v, _ := conf.GetPref(keyTempPath); v != filepath.Join(real, "incomplete")+"/" {
		t.Fatalf("临时目录没跟着走：%q", v)
	}
	if v, _ := conf.GetPref(keyBTPort); v != "27000" {
		t.Fatalf("BT 端口没更新：%q", v)
	}
}

// 参数没变时绝不能重写配置 —— 那些设置在 WebUI 里也能改，
// 每次启动照参数刷一遍会把用户在 WebUI 里的修改冲掉。
func TestApplyParamsDoesNotClobberWebUIEdits(t *testing.T) {
	l := newTestLayout(t)
	p := loadParams(lookupFrom(map[string]string{envDownloadPath: t.TempDir()}))
	st, _ := applyParams(l, p, state{})
	st.Initialized = true

	// 模拟用户在 WebUI 里把保存路径改到别处
	conf := readConf(t, l)
	conf.SetPref(keySavePath, "/用户/自己/改的/")
	if err := os.WriteFile(l.confPath, conf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := applyParams(l, p, st); err != nil { // 同样的参数再来一次
		t.Fatal(err)
	}
	if v, _ := readConf(t, l).GetPref(keySavePath); v != "/用户/自己/改的/" {
		t.Fatalf("用户在 WebUI 里的修改被覆盖了：%q", v)
	}
}

// 参数指向的目录还没被平台挂进沙箱时，宁可不动配置，也别把好端端的
// 已有配置改成兜底路径。
func TestApplyParamsKeepsConfigWhenPathUnusable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑时权限位拦不住，跳过")
	}
	l := newTestLayout(t)
	good := t.TempDir()
	st, _ := applyParams(l, loadParams(lookupFrom(map[string]string{envDownloadPath: good})), state{})
	st.Initialized = true

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(locked, "dl")
	st2, err := applyParams(l, loadParams(lookupFrom(map[string]string{envDownloadPath: bad})), st)
	if err != nil {
		t.Fatal(err)
	}
	if st2.AppliedDownloadPath != good {
		t.Fatalf("不该把不可写的目录记成已应用：%q", st2.AppliedDownloadPath)
	}
	if v, _ := readConf(t, l).GetPref(keySavePath); v != good+"/" {
		t.Fatalf("原有保存路径被改坏了：%q", v)
	}
}

// 配置里已经有密码（用户在 WebUI 里设的）时，绝不能覆盖它。
func TestApplyParamsNeverOverwritesExistingPassword(t *testing.T) {
	l := newTestLayout(t)
	st, _ := applyParams(l, loadParams(lookupFrom(nil)), state{})
	st.Initialized = true

	conf := readConf(t, l)
	conf.Set("Preferences", `WebUI\Password_PBKDF2`, `"@ByteArray(用户自己设的)"`)
	if err := os.WriteFile(l.confPath, conf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyParams(l, loadParams(lookupFrom(nil)), st); err != nil {
		t.Fatal(err)
	}
	if v, _ := readConf(t, l).Get("Preferences", `WebUI\Password_PBKDF2`); v != `"@ByteArray(用户自己设的)"` {
		t.Fatalf("用户自己设的密码被覆盖了：%q", v)
	}
}

// 状态文件丢了（损坏、被清过）但配置还在时，不能把密码重置掉。
func TestApplyParamsLostStateKeepsExistingPassword(t *testing.T) {
	l := newTestLayout(t)
	st, _ := applyParams(l, loadParams(lookupFrom(nil)), state{})
	st.Initialized = true
	before, _ := readConf(t, l).Get("Preferences", `WebUI\Password_PBKDF2`)

	if _, err := applyParams(l, loadParams(lookupFrom(nil)), state{}); err != nil { // 状态归零
		t.Fatal(err)
	}
	after, _ := readConf(t, l).Get("Preferences", `WebUI\Password_PBKDF2`)
	if before != after {
		t.Fatal("状态丢失不该导致密码被重置")
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := state{Initialized: true, AppliedDownloadPath: "/volume1/dl", AppliedBTPort: 26881}
	if err := saveState(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := loadState(dir); got != want {
		t.Fatalf("状态没往返回来：%+v", got)
	}
	// 文件坏掉时按"没有状态"处理，别让应用起不来。
	if err := os.WriteFile(statePath(dir), []byte("{坏掉的"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadState(dir); got.Initialized {
		t.Fatal("坏掉的状态文件不该被当成已初始化")
	}
}
