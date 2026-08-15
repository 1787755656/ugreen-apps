package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestINISetExistingKeyInPlace(t *testing.T) {
	f := parseINI([]byte("[A]\nx=1\n\n[B]\ny=2\n"))
	f.Set("A", "x", "9")
	got := string(f.Bytes())
	want := "[A]\nx=9\n\n[B]\ny=2\n"
	if got != want {
		t.Fatalf("就地替换出错\n got=%q\nwant=%q", got, want)
	}
}

func TestINISetAppendsToRightSection(t *testing.T) {
	f := parseINI([]byte("[A]\nx=1\n\n[B]\ny=2\n"))
	f.Set("A", "z", "3")
	got := string(f.Bytes())
	want := "[A]\nx=1\nz=3\n\n[B]\ny=2\n"
	if got != want {
		t.Fatalf("追加到目标节出错\n got=%q\nwant=%q", got, want)
	}
}

func TestINISetCreatesSection(t *testing.T) {
	f := parseINI([]byte("[A]\nx=1\n"))
	f.Set("C", "k", "v")
	got := string(f.Bytes())
	if !strings.Contains(got, "[C]\nk=v\n") {
		t.Fatalf("没有新建节：%q", got)
	}
	if _, ok := f.Get("A", "x"); !ok {
		t.Fatal("新建节把原有内容弄丢了")
	}
}

// 那条 15KB 的 tracker 行和 @ByteArray 密码是最不能被"顺手规范化"的两样，
// 编辑其它键时必须逐字节原样保留。
func TestINIPreservesExoticValues(t *testing.T) {
	tracker := `Bittorrent\TrackersList=http://a/announce\nhttp://b/announce`
	pw := `WebUI\Password_PBKDF2="@ByteArray(YWJj:ZGVm)"`
	src := "[Preferences]\n" + tracker + "\n" + pw + "\nGeneral\\Locale=zh_CN\n"
	f := parseINI([]byte(src))
	f.Set("Preferences", `Connection\PortRangeMin`, "26881")

	got := string(f.Bytes())
	for _, keep := range []string{tracker, pw} {
		if !strings.Contains(got, keep) {
			t.Fatalf("原样保留失败，丢了：%s\n结果：%s", keep, got)
		}
	}
	if v, _ := f.Get("Preferences", `Connection\PortRangeMin`); v != "26881" {
		t.Fatalf("端口没写进去：%q", v)
	}
}

// qBittorrent 5.x 会把老键迁移进 [BitTorrent] 并从此只读新键。
// 只改老键 = 完全不生效，而且不报错 —— 这是本项目最容易静默失效的一处。
func TestSetPrefWritesModernKeysOnceMigrated(t *testing.T) {
	migrated := "[BitTorrent]\nSession\\DefaultSavePath=/old/\n\n[Meta]\nMigrationVersion=8\n\n" +
		"[Preferences]\nDownloads\\SavePath=/old/\n"
	f := parseINI([]byte(migrated))
	if !f.migrated() {
		t.Fatal("没认出这是迁移过的配置")
	}
	f.SetPref(keySavePath, "/volume1/dl/")

	if v, _ := f.Get("BitTorrent", `Session\DefaultSavePath`); v != "/volume1/dl/" {
		t.Fatalf("新键没更新：%q", v)
	}
	if v, _ := f.Get("Preferences", `Downloads\SavePath`); v != "/volume1/dl/" {
		t.Fatalf("老键没跟着更新：%q", v)
	}
}

func TestSetPrefOnFreshConfigOnlyTouchesLegacy(t *testing.T) {
	// 镜像里带的默认配置就是这种"还没被迁移过"的形态。
	f := parseINI(defaultConf)
	if f.migrated() {
		t.Fatal("镜像自带的默认配置不该被判成已迁移")
	}
	f.SetPref(keyBTPort, "26881")
	if v, _ := f.Get("Preferences", `Connection\PortRangeMin`); v != "26881" {
		t.Fatalf("老键没写：%q", v)
	}
	if f.HasSection("BitTorrent") {
		t.Fatal("不该在未迁移的配置里凭空造出 [BitTorrent] 节，那会被迁移覆盖")
	}
}

func TestGetPrefPrefersModernAfterMigration(t *testing.T) {
	f := parseINI([]byte("[BitTorrent]\nSession\\Port=26881\n\n[Meta]\nMigrationVersion=8\n\n" +
		"[Preferences]\nConnection\\PortRangeMin=6881\n"))
	if v, _ := f.GetPref(keyBTPort); v != "26881" {
		t.Fatalf("迁移后应以新键为准，得到 %q", v)
	}
}

// 拿真机上 qBittorrent 5.2.3 自己写出来的那条密码逐字节对比，
// 确认我们手搓的 PBKDF2 参数（SHA-512 / 100000 轮 / 64 字节）和它一致。
func TestPasswordMatchesRealQbittorrentOutput(t *testing.T) {
	const real = `"@ByteArray(eUjNz1YtVzIo2s/0oHDd5w==:nLZYJtUp4BkYzOZn0mky8kSA3oAoOuJ9V1LnPzRARYQae9ni6zhs2eV+yg8il/LfzDObC4dCqDSvFlfL+DVJyg==)"`
	salt, err := base64.StdEncoding.DecodeString("eUjNz1YtVzIo2s/0oHDd5w==")
	if err != nil {
		t.Fatal(err)
	}
	if got := qbPasswordValueWithSalt("adminadmin", salt); got != real {
		t.Fatalf("和真机产物不一致\n got=%s\nwant=%s", got, real)
	}
}

func TestPasswordValueShapeAndRandomSalt(t *testing.T) {
	a, err := qbPasswordValue("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := qbPasswordValue("hunter2")
	if a == b {
		t.Fatal("两次生成的盐相同，说明盐没随机")
	}
	if !strings.HasPrefix(a, `"@ByteArray(`) || !strings.HasSuffix(a, `)"`) {
		t.Fatalf("外层格式不对：%s", a)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(a, `"@ByteArray(`), `)"`)
	parts := strings.Split(inner, ":")
	if len(parts) != 2 {
		t.Fatalf("应为 盐:密钥 两段，得到 %d 段", len(parts))
	}
	salt, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || len(salt) != pbkdf2SaltLen {
		t.Fatalf("盐应为 %d 字节，得到 %d (%v)", pbkdf2SaltLen, len(salt), err)
	}
	dk, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(dk) != pbkdf2KeyLen {
		t.Fatalf("密钥应为 %d 字节，得到 %d (%v)", pbkdf2KeyLen, len(dk), err)
	}
}
