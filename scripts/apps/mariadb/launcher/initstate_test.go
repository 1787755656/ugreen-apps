package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一整组测试守的是同一个 bug：
// 账号类语句（CREATE USER / ALTER USER / GRANT）在 `mariadbd --bootstrap` 下会被
// 拒绝并报 1290，因为 --bootstrap 隐含 --skip-grant-tables。0003 之前把账号 SQL
// 拼在系统表脚本后面一起丢给 bootstrap，结果每一次全新安装都装出一个
// "有系统表、没有 root 账号" 的数据库，登录一律 1045。

// FLUSH PRIVILEGES 必须是【第一句】。临时实例是 --skip-grant-tables 起的，
// 不先把授权系统打开，后面的 CREATE USER 照样 1290 —— 换了执行通道也救不回来。
func TestRootPasswordSQLStartsWithFlushPrivileges(t *testing.T) {
	sql, err := rootPasswordSQL("a-good-password")
	if err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSpace(strings.SplitN(sql, "\n", 2)[0])
	if first != "FLUSH PRIVILEGES;" {
		t.Fatalf("第一句是 %q，必须是 FLUSH PRIVILEGES; —— "+
			"否则在 --skip-grant-tables 的临时实例里建账号会报 1290", first)
	}
}

// 数据目录三档的判定。第二档（InitAccountsMissing）就是被这个 bug 坑过的存量机器，
// 必须能被识别出来并自愈，否则用户只能重装、还会丢数据。
func TestClassifyDataDir(t *testing.T) {
	mkMysqlDir := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "mysql"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mysql", "global_priv.frm"),
			[]byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("空目录要完整初始化", func(t *testing.T) {
		if got := classifyDataDir(t.TempDir()); got != InitFresh {
			t.Fatalf("got %v, want InitFresh", got)
		}
	})

	t.Run("mysql 目录存在但没有标记 = 账号没建完", func(t *testing.T) {
		d := t.TempDir()
		mkMysqlDir(t, d)
		if got := classifyDataDir(d); got != InitAccountsMissing {
			t.Fatalf("got %v, want InitAccountsMissing —— "+
				"这正是 0003 之前装出来的库的状态，认不出来就没法自愈", got)
		}
	})

	t.Run("有标记 = 完好", func(t *testing.T) {
		d := t.TempDir()
		mkMysqlDir(t, d)
		if err := os.WriteFile(filepath.Join(d, initDoneMarker), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		if got := classifyDataDir(d); got != InitComplete {
			t.Fatalf("got %v, want InitComplete", got)
		}
	})

	// mysql 目录空着不能算"已初始化"—— 老判据是 ReadDir 非空，这里守住边界。
	t.Run("mysql 目录存在但是空的 = 还得重来", func(t *testing.T) {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, "mysql"), 0o750); err != nil {
			t.Fatal(err)
		}
		if got := classifyDataDir(d); got != InitFresh {
			t.Fatalf("got %v, want InitFresh", got)
		}
	})
}

// 1045 的判定按错误号来，别匹配英文文案 —— 文案随语言包和版本变。
func TestIsAuthFailure(t *testing.T) {
	yes := []string{
		"ERROR 1045 (28000): Access denied for user 'root'@'localhost' (using password: YES)",
		"Got the following error: ERROR 1045 (28000)",
	}
	no := []string{
		"ERROR 2002 (HY000): Can't connect to local server through socket",
		"Phase 1/7: Checking and upgrading mysql database",
		"",
	}
	for _, s := range yes {
		if !isAuthFailure(s) {
			t.Errorf("应当识别为认证失败: %q", s)
		}
	}
	for _, s := range no {
		if isAuthFailure(s) {
			t.Errorf("不该识别为认证失败: %q", s)
		}
	}
}

// 维护用临时实例的三条参数不变量。
func TestTempServerArgs(t *testing.T) {
	s := &Server{
		p:       &Paths{Base: "/base", LibDir: "/base/lib", ShareDir: "/base/share", Cache: "/cache"},
		dataDir: "/data",
	}
	args := strings.Join(s.tempServerArgs("/cache/tmp/init.sock"), " ")

	for _, must := range []string{"--skip-grant-tables", "--skip-networking",
		"--socket=/cache/tmp/init.sock", "--datadir=/data"} {
		if !strings.Contains(args, must) {
			t.Errorf("缺少 %s：\n%s", must, args)
		}
	}
	// --bootstrap 混进来就等于退回那个 1290 的老 bug：账号语句会被静默拒绝，
	// 装出一个登录必报 1045 的数据库。
	if strings.Contains(args, "--bootstrap") {
		t.Errorf("临时实例【不能】带 --bootstrap —— 那个模式下建不了账号：\n%s", args)
	}
}
