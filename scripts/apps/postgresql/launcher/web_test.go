package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 备份文件名是唯一一处"用户给的字符串直接参与拼路径"的地方。
// 正则是第一道闸，filepath.Base 是第二道 —— 两道都要真的挡得住。
func TestBackupPathRejectsTraversal(t *testing.T) {
	a := &App{backups: "/data/backups"}

	bad := []string{
		"../../../etc/passwd",
		"../config.json",
		"/etc/shadow",
		"a/../../b.sql.gz",
		"..%2F..%2Fx.sql.gz",
		"x.sql.gz\n",
		"",
		"x.sql",          // 后缀不对
		"x.sql.gz.other", // 后缀不在末尾
		strings.Repeat("a", 200) + ".sql.gz",
	}
	for _, name := range bad {
		if p, ok := a.backupPath(name); ok {
			t.Errorf("backupPath(%q) 放行了，算出 %q", name, p)
		}
	}

	good := []string{"dumpall-20260808-120000.sql.gz", "my_backup.sql.gz", "a.sql.gz"}
	for _, name := range good {
		p, ok := a.backupPath(name)
		if !ok {
			t.Errorf("backupPath(%q) 不该被拒", name)
			continue
		}
		if filepath.Dir(p) != a.backups {
			t.Errorf("backupPath(%q) = %q，跑到备份目录外面去了", name, p)
		}
	}
}

// 库名/角色名直接进 SQL（带引号），但我们仍然把字符集卡死 ——
// 引号转义写对了是一回事，不给自己留出错的机会是另一回事。
func TestNameRe(t *testing.T) {
	bad := []string{
		"", "1abc", "a-b", "a b", "a;DROP DATABASE postgres;--",
		`a"b`, "a'b", "库名", strings.Repeat("a", 64),
	}
	for _, n := range bad {
		if nameRe.MatchString(n) {
			t.Errorf("nameRe 放行了 %q", n)
		}
	}
	for _, n := range []string{"immich", "_x", "a1", "n8n_db", strings.Repeat("a", 63)} {
		if !nameRe.MatchString(n) {
			t.Errorf("nameRe 拒绝了合法的 %q", n)
		}
	}
}

// 上传会话号也参与拼路径。
func TestUploadDirRejectsBadSession(t *testing.T) {
	a := &App{uploads: "/data/uploads"}
	for _, s := range []string{"", "..", "../x", "ABCDEF0123456789abcdef0123456789",
		"0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdef0"} {
		if p, ok := a.uploadDir(s); ok {
			t.Errorf("uploadDir(%q) 放行了，算出 %q", s, p)
		}
	}
	p, ok := a.uploadDir("0123456789abcdef0123456789abcdef")
	if !ok || filepath.Dir(p) != a.uploads {
		t.Errorf("合法会话号被拒或算错了路径：%q %v", p, ok)
	}
}

// SQL 字面量/标识符的引号处理。standard_conforming_strings 默认 on，
// 所以反斜杠是普通字符，只需要把引号翻倍 —— 但必须真的翻倍。
func TestQuoting(t *testing.T) {
	if got := quoteLiteral("it's"); got != "'it''s'" {
		t.Errorf("quoteLiteral = %q", got)
	}
	if got := quoteLiteral(`a\'; DROP`); got != `'a\''; DROP'` {
		t.Errorf("quoteLiteral = %q", got)
	}
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %q", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.0 KiB",
		1536: "1.5 KiB", 1 << 20: "1.0 MiB", 3 << 30: "3.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// 分片上限是由总量上限推出来的，不能是两个各写各的魔数 ——
// 分开写的话改了一个忘了另一个，要么传不上大文件，要么上限形同虚设。
func TestUploadLimitsAreConsistent(t *testing.T) {
	if int64(uploadMaxChunks)*uploadChunkSize < maxUploadBytes {
		t.Fatalf("分片数上限 %d × 分片大小 %d 装不下 %d 的总上限",
			uploadMaxChunks, uploadChunkSize, maxUploadBytes)
	}
}
