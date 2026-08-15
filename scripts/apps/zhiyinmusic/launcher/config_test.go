package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 上游 config.toml 的缩样：带中文注释、带跨行数组、带嵌套表。
// 真实那份两百多行，形状就是这样。
const templateTOML = `# 知音配置文件示例
[server]
# 监听地址
host = "0.0.0.0"
port = 8080
drop_cache_after_stream = false

[scan]
# Docker 部署示例
roots = ["/music"]
mode = "manual"
concurrency = 2

[database]
path = "/data/db.sqlite"
pool_size = 4

[covers]
cache_path = "/covers"
standard_names = [
    "cover",
    "folder",
]
quality = 85

[transcode]
enabled = true
cache_path = "/data/transcoded"

[recommend.weights]
frequency = 0.4

[web]
enabled = true
path = "/app/web"
`

func testPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{
		Root:  root,
		Bin:   filepath.Join(root, "bin"),
		Lib:   filepath.Join(root, "lib"),
		App:   filepath.Join(root, "app"),
		Data:  filepath.Join(root, "data"),
		Cache: filepath.Join(root, "cache"),
	}
	p.Server = filepath.Join(p.Bin, "zhiyin-music")
	p.Web = filepath.Join(p.App, "web")
	if err := os.MkdirAll(p.App, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.ensureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigTemplate(), []byte(templateTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readConfig(t *testing.T, p Paths) string {
	t.Helper()
	b, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	return string(b)
}

// 首次启动：按模板生成，所有受管键都被掰到沙箱里的可写位置。
func TestSyncConfigFresh(t *testing.T) {
	p := testPaths(t)
	params := Params{MusicPaths: []string{"/volume1/media/音乐"}}

	added, removed, err := syncConfig(p, 28085, params)
	if err != nil {
		t.Fatalf("syncConfig: %v", err)
	}
	if len(added) != 1 || added[0] != "/volume1/media/音乐" {
		t.Errorf("added = %v，期望只有那一个音乐目录", added)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v，首次不该有移出", removed)
	}

	got := readConfig(t, p)
	for _, want := range []string{
		`port = 28085`,
		`host = "0.0.0.0"`,
		`path = ` + quote(p.Database()),
		`cache_path = ` + quote(p.CoverStore()),
		`cache_path = ` + quote(p.TranscodeCache()),
		`path = ` + quote(p.Web),
		`roots = ["/volume1/media/音乐"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("生成的配置里缺 %q\n---\n%s", want, got)
		}
	}

	// Docker 里的那几个路径一个都不该留下 —— 留下就是往沙箱里写不进去的地方写。
	for _, bad := range []string{`"/data/db.sqlite"`, `"/covers"`, `"/data/transcoded"`, `"/app/web"`, `"/music"`} {
		if strings.Contains(got, bad) {
			t.Errorf("生成的配置里还留着 Docker 路径 %s", bad)
		}
	}

	// 注释和用户可调项必须原样保留 —— 用户在绿联上没有 SSH，
	// 这些中文注释是他唯一能看到的配置说明。
	for _, want := range []string{"# 知音配置文件示例", "# 监听地址", "pool_size = 4", "quality = 85", "frequency = 0.4"} {
		if !strings.Contains(got, want) {
			t.Errorf("丢了原文内容 %q", want)
		}
	}
	// 跨行数组不能被吃掉
	if !strings.Contains(got, `"cover",`) || !strings.Contains(got, `"folder",`) {
		t.Errorf("跨行数组 standard_names 被破坏了：\n%s", got)
	}
}

// 二次启动：用户在应用里改过的配置必须原样保留，只有受管键跟着变。
func TestSyncConfigPreservesUserEdits(t *testing.T) {
	p := testPaths(t)
	if _, _, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a"}}); err != nil {
		t.Fatal(err)
	}

	// 模拟用户在设置界面里改了并发和缓存策略
	cfg := readConfig(t, p)
	cfg = strings.Replace(cfg, "concurrency = 2", "concurrency = 8", 1)
	cfg = strings.Replace(cfg, "pool_size = 4", "pool_size = 2", 1)
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a"}}); err != nil {
		t.Fatal(err)
	}
	got := readConfig(t, p)
	if !strings.Contains(got, "concurrency = 8") || !strings.Contains(got, "pool_size = 2") {
		t.Errorf("用户改过的配置被冲掉了：\n%s", got)
	}
	if strings.Count(got, "roots =") != 1 {
		t.Errorf("roots 被写重复了：\n%s", got)
	}
}

// 用户在应用里自己加的目录不能被我们移掉；参数里去掉的（且当初是我们加的）要收敛。
func TestMergeRootsKeepsUserAddedRemovesOurs(t *testing.T) {
	p := testPaths(t)

	if _, _, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a", "/volume1/b"}}); err != nil {
		t.Fatal(err)
	}
	// 用户在应用里又加了一个我们完全不知道的目录
	cfg := strings.Replace(readConfig(t, p),
		`roots = ["/volume1/a", "/volume1/b"]`,
		`roots = ["/volume1/a", "/volume1/b", "/volume1/user-added"]`, 1)
	if err := os.WriteFile(p.ConfigFile(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// 用户把 b 从安装参数里去掉了
	added, removed, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v，不该有新增", added)
	}
	if len(removed) != 1 || removed[0] != "/volume1/b" {
		t.Errorf("removed = %v，期望只移出 /volume1/b", removed)
	}
	roots := parseStringArray(readConfig(t, p), "scan", "roots")
	want := []string{"/volume1/a", "/volume1/user-added"}
	if strings.Join(roots, "|") != strings.Join(want, "|") {
		t.Errorf("roots = %v，期望 %v（用户自己加的必须留着）", roots, want)
	}
}

// 一个目录都没选（首启必然如此）要能生成合法配置，不能崩、也不能留下 /music。
func TestSyncConfigNoParams(t *testing.T) {
	p := testPaths(t)
	if _, _, err := syncConfig(p, 28085, Params{}); err != nil {
		t.Fatalf("首启拿不到参数时不该失败：%v", err)
	}
	got := readConfig(t, p)
	if !strings.Contains(got, "roots = []") {
		t.Errorf("期望 roots = []，实际：\n%s", got)
	}
}

// 上游把配置整份重写回来（PUT /api/config，注释没了、数组变跨行）之后，
// 我们还得能认出并改对受管键。
func TestApplyManagedOnSerializedConfig(t *testing.T) {
	serialized := `[server]
host = "0.0.0.0"
port = 28085

[scan]
roots = [
    "/volume1/a",
    "/volume1/b",
]
mode = "watch"

[web]
enabled = true
path = "/old/web"
`
	out := applyManaged(serialized, []managedKey{
		{"scan", "roots", tomlStringArray([]string{"/volume1/c"})},
		{"web", "path", `"/new/web"`},
	})
	if !strings.Contains(out, `roots = ["/volume1/c"]`) {
		t.Errorf("跨行数组没被整段替换：\n%s", out)
	}
	if strings.Contains(out, `"/volume1/a"`) || strings.Contains(out, `"/volume1/b"`) {
		t.Errorf("旧的跨行数组元素有残留（会变成语法错误）：\n%s", out)
	}
	if !strings.Contains(out, `path = "/new/web"`) || strings.Contains(out, `"/old/web"`) {
		t.Errorf("web.path 没换掉：\n%s", out)
	}
	if !strings.Contains(out, `mode = "watch"`) {
		t.Errorf("跨行数组后面的行被吃掉了：\n%s", out)
	}
}

// section 或 key 缺失时要补出来，而不是静默丢掉这条设置。
func TestApplyManagedInsertsMissing(t *testing.T) {
	out := applyManaged("[server]\nhost = \"0.0.0.0\"\n", []managedKey{
		{"server", "port", "28085"},
		{"database", "path", `"/data/db.sqlite"`},
	})
	if !strings.Contains(out, "port = 28085") {
		t.Errorf("缺的 key 没补进已有 section：\n%s", out)
	}
	if !strings.Contains(out, "[database]") || !strings.Contains(out, `path = "/data/db.sqlite"`) {
		t.Errorf("缺的 section 没补出来：\n%s", out)
	}
	// 补出来的 port 必须落在 [server] 里，不能跑到 [database] 后面去
	if strings.Index(out, "port = 28085") > strings.Index(out, "[database]") {
		t.Errorf("补的 key 落到别的 section 里了：\n%s", out)
	}
}

// 同名 key 出现在多个 section 里（path、cache_path 都是），不能改错地方。
func TestApplyManagedSameKeyDifferentSections(t *testing.T) {
	src := `[database]
path = "/old/db"

[web]
path = "/old/web"
`
	out := applyManaged(src, []managedKey{
		{"database", "path", `"/new/db"`},
		{"web", "path", `"/new/web"`},
	})
	if !strings.Contains(out, `path = "/new/db"`) || !strings.Contains(out, `path = "/new/web"`) {
		t.Errorf("同名 key 没各改各的：\n%s", out)
	}
	if strings.Contains(out, "/old/") {
		t.Errorf("有旧值残留：\n%s", out)
	}
}

func TestParseStringArray(t *testing.T) {
	cases := []struct {
		name, src string
		want      []string
	}{
		{"单行", "[scan]\nroots = [\"/a\", \"/b\"]\n", []string{"/a", "/b"}},
		{"跨行", "[scan]\nroots = [\n  \"/a\",\n  \"/b\",\n]\n", []string{"/a", "/b"}},
		{"空数组", "[scan]\nroots = []\n", nil},
		{"别的 section 同名", "[other]\nroots = [\"/x\"]\n[scan]\nroots = [\"/a\"]\n", []string{"/a"}},
		{"带注释", "[scan]\nroots = [\"/a\"] # 注释里有 \"/b\"\n", []string{"/a"}},
		{"路径里有空格和中文", "[scan]\nroots = [\"/volume1/我的 音乐\"]\n", []string{"/volume1/我的 音乐"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStringArray(c.src, "scan", "roots")
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// 生成的配置必须是【上游真的能解析】的 TOML。这里做一遍粗校验：
// 每个非空非注释行要么是 section 头，要么是 key = value，要么在跨行数组里。
func TestGeneratedConfigIsWellFormed(t *testing.T) {
	p := testPaths(t)
	if _, _, err := syncConfig(p, 28085, Params{MusicPaths: []string{"/volume1/a", "/volume1/b"}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(readConfig(t, p), "\n")
	pending := ""
	for i, line := range lines {
		if pending != "" {
			pending += "\n" + line
			if valueComplete(pending) {
				pending = ""
			}
			continue
		}
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || sectionRe.MatchString(line) {
			continue
		}
		if !strings.Contains(s, "=") {
			t.Fatalf("第 %d 行不像合法 TOML：%q", i+1, line)
		}
		if !valueComplete(s) {
			pending = s
		}
	}
	if pending != "" {
		t.Errorf("文件结束时还有没闭合的数组：%q", pending)
	}
}

// JWT 密钥：首次要生成一个持久的，之后【绝不能】再变。
//
// 不做这件事的话上游每次启动都随机生成一把，所有人的登录态在应用重启后全部失效
// —— 而绿联这边光是"首启参数为空、停一次再启动"的流程就至少重启一次。
func TestJWTSecretGeneratedOnceAndKept(t *testing.T) {
	p := testPaths(t)
	if _, _, err := syncConfig(p, 28085, Params{}); err != nil {
		t.Fatal(err)
	}
	first := parseScalarString(readConfig(t, p), "security", "jwt_secret")
	if len(first) != 64 {
		t.Fatalf("首次没生成 64 位十六进制密钥，实际 %q", first)
	}

	for i := 0; i < 3; i++ {
		if _, _, err := syncConfig(p, 28085, Params{}); err != nil {
			t.Fatal(err)
		}
		if got := parseScalarString(readConfig(t, p), "security", "jwt_secret"); got != first {
			t.Fatalf("第 %d 次启动密钥变了（所有人会被登出）：%q → %q", i+2, first, got)
		}
	}
}

// 用户自己在配置里写了密钥（或者从别处迁移过来的），必须原样保留。
func TestJWTSecretUserProvidedKept(t *testing.T) {
	p := testPaths(t)
	tpl := strings.Replace(templateTOML, "[web]", "[security]\njwt_secret = \"user-chosen-secret\"\n\n[web]", 1)
	if err := os.WriteFile(p.ConfigTemplate(), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := syncConfig(p, 28085, Params{}); err != nil {
		t.Fatal(err)
	}
	if got := parseScalarString(readConfig(t, p), "security", "jwt_secret"); got != "user-chosen-secret" {
		t.Errorf("用户自己配的密钥被覆盖了：%q", got)
	}
}

// 模板里 jwt_secret 是【注释掉】的，不能被当成"已经有了"。
func TestCommentedKeyIsNotFound(t *testing.T) {
	src := "[security]\n# jwt_secret = \"your-random-hex-secret-here\"\ntoken_expiry_hours = 168\n"
	if got := parseScalarString(src, "security", "jwt_secret"); got != "" {
		t.Errorf("注释掉的 key 被当成有值了：%q", got)
	}
}

// 更新日志要放到上游能读到的位置（相对 cwd 的 releases/releases.json）。
func TestSyncReleases(t *testing.T) {
	p := testPaths(t)
	if err := p.syncReleases(); err == nil {
		t.Error("包里没有 releases.json 时应当报错，好让调用方记一条日志")
	}
	if err := os.WriteFile(p.ReleasesSrc(), []byte(`[{"version":"0.8.1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.syncReleases(); err != nil {
		t.Fatalf("syncReleases: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(p.Data, "releases", "releases.json"))
	if err != nil {
		t.Fatalf("上游会去 <数据目录>/releases/releases.json 读，那里应该有文件：%v", err)
	}
	if !strings.Contains(string(b), "0.8.1") {
		t.Errorf("内容不对：%s", b)
	}
}
