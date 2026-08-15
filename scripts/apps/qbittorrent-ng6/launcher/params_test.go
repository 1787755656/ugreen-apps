package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lookupFrom(m map[string]string) lookupFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestParamsDefaultsWhenNothingSet(t *testing.T) {
	p := loadParams(lookupFrom(nil))
	if p.BTPort != defaultBTPort {
		t.Fatalf("BT 端口默认值错：%d", p.BTPort)
	}
	if !p.TrackerUpdate {
		t.Fatal("tracker 更新默认应为开")
	}
	if p.TrackerURL != defaultTrackerURL {
		t.Fatalf("tracker 地址默认值错：%s", p.TrackerURL)
	}
	if p.DownloadPath != "" {
		t.Fatal("没设的参数不该凭空有值")
	}
}

// 装完的第一次启动，平台还没写 .env，所有参数都是空的 —— 这是正常状态，
// 不能有任何一项被当成错误。
func TestParamsFirstBootAllEmpty(t *testing.T) {
	p := loadParams(lookupFrom(map[string]string{
		envDownloadPath: "", envBTPort: "", envTrackerUpdate: "", envTrackerURL: "",
	}))
	if len(p.notes) != 0 {
		t.Fatalf("首启空值不该产生任何提示：%v", p.notes)
	}
	if p.BTPort != defaultBTPort {
		t.Fatalf("应回落到默认端口，得到 %d", p.BTPort)
	}
}

// type: path 一个都没选时平台写的是字面量 null，不是空串。
func TestParamsPathNullTreatedAsUnset(t *testing.T) {
	p := loadParams(lookupFrom(map[string]string{envDownloadPath: "null"}))
	if p.DownloadPath != "" {
		t.Fatalf("null 应视为未设置，得到 %q", p.DownloadPath)
	}
	if len(p.notes) != 0 {
		t.Fatalf("null 是正常情况，不该有提示：%v", p.notes)
	}
}

// type: number 留空时平台可能写成 0 —— 当成"用户填了 0"就会撞上下限校验。
func TestParamsNumberZeroTreatedAsUnset(t *testing.T) {
	p := loadParams(lookupFrom(map[string]string{envBTPort: "0"}))
	if p.BTPort != defaultBTPort {
		t.Fatalf("0 应视为未设置，得到 %d", p.BTPort)
	}
	if len(p.notes) != 0 {
		t.Fatalf("0 是留空的正常表示，不该有提示：%v", p.notes)
	}
}

// 一个参数不合法，绝不能连累其它已经解析好的参数。
func TestParamsBadValuesDoNotPoisonGoodOnes(t *testing.T) {
	p := loadParams(lookupFrom(map[string]string{
		envDownloadPath:  "/volume1/dl",
		envBTPort:        "80", // <1024，沙箱里 bind 不了
		envTrackerUpdate: "随便写的",
		envTrackerURL:    "ftp://x/y",
	}))
	if p.DownloadPath != "/volume1/dl" {
		t.Fatalf("合法的下载目录被连累了：%q", p.DownloadPath)
	}
	if p.BTPort != defaultBTPort {
		t.Fatalf("越界端口应回落默认，得到 %d", p.BTPort)
	}
	if !p.TrackerUpdate {
		t.Fatal("无法识别的开关应按默认 true 处理")
	}
	if p.TrackerURL != defaultTrackerURL {
		t.Fatalf("非 http 地址应回落内置地址，得到 %s", p.TrackerURL)
	}
	if len(p.notes) != 3 {
		t.Fatalf("三处不合法应各留一条说明，得到 %d 条：%v", len(p.notes), p.notes)
	}
}

func TestParamsRelativeDownloadPathRejected(t *testing.T) {
	p := loadParams(lookupFrom(map[string]string{envDownloadPath: "downloads"}))
	if p.DownloadPath != "" {
		t.Fatalf("相对路径应被拒，得到 %q", p.DownloadPath)
	}
	if len(p.notes) != 1 {
		t.Fatalf("应留一条说明，得到 %v", p.notes)
	}
}

func TestParamsTrackerToggleForms(t *testing.T) {
	for _, v := range []string{"false", "0", "no", "off", "FALSE"} {
		if loadParams(lookupFrom(map[string]string{envTrackerUpdate: v})).TrackerUpdate {
			t.Fatalf("%q 应关闭 tracker 更新", v)
		}
	}
	for _, v := range []string{"true", "1", "yes", "on"} {
		if !loadParams(lookupFrom(map[string]string{envTrackerUpdate: v})).TrackerUpdate {
			t.Fatalf("%q 应开启 tracker 更新", v)
		}
	}
}

func TestNormalizeTrackerList(t *testing.T) {
	got := normalizeTrackerList("http://a/announce\n\n  http://b/announce  \r\n\nudp://c:80/announce\n")
	want := `http://a/announce\nhttp://b/announce\nudp://c:80/announce`
	if got != want {
		t.Fatalf("\n got=%q\nwant=%q", got, want)
	}
}

// Qt 的 INI 拿逗号当 QStringList 分隔符，混进来会把整条值切碎。
func TestNormalizeTrackerListDropsCommaLines(t *testing.T) {
	got := normalizeTrackerList("http://ok/announce\nhttp://bad/a,b\nhttp://ok2/announce")
	if strings.Contains(got, "bad") {
		t.Fatalf("带逗号的行应被丢掉：%q", got)
	}
	if !strings.Contains(got, "ok2") {
		t.Fatalf("正常行不该被误伤：%q", got)
	}
}

func TestEnsureWritableDirDetectsUnwritable(t *testing.T) {
	base := t.TempDir()
	ro := filepath.Join(base, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("以 root 跑时权限位拦不住，跳过")
	}
	// 目录存在但不可写：只 Stat 会说"没问题"，必须真的试写。
	if err := ensureWritableDir(filepath.Join(ro, "sub")); err == nil {
		t.Fatal("不可写目录应当报错")
	}
	if err := ensureWritableDir(filepath.Join(base, "ok", "nested")); err != nil {
		t.Fatalf("可写目录应当成功：%v", err)
	}
}
