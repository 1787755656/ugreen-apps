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
		"a/../../b.rdb.gz",
		"..%2F..%2Fx.rdb.gz",
		"x.rdb.gz\n",
		"",
		"x.rdb",          // 后缀不对
		"x.rdb.gz.other", // 后缀不在末尾
		"dump.sql.gz",    // 别的应用的备份
		strings.Repeat("a", 200) + ".rdb.gz",
	}
	for _, name := range bad {
		if p, ok := a.backupPath(name); ok {
			t.Errorf("backupPath(%q) 放行了，算出 %q", name, p)
		}
	}

	good := []string{"dump-20260808-120000.rdb.gz", "my_backup.rdb.gz", "a.rdb.gz"}
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

// 分片上限是由总量上限推出来的，不能是两个各写各的魔数 ——
// 分开写的话改了一个忘了另一个，要么传不上大文件，要么上限形同虚设。
func TestUploadLimitsAreConsistent(t *testing.T) {
	if int64(uploadMaxChunks)*uploadChunkSize < maxUploadBytes {
		t.Fatalf("分片数上限 %d × 分片大小 %d 装不下 %d 的总上限",
			uploadMaxChunks, uploadChunkSize, maxUploadBytes)
	}
}

// 默认密码必须是随机的、够长的。Redis 无密码 + 绑 0.0.0.0 是历史上
// 被入侵最多的组合之一，默认值这一关不能松。
func TestDefaultConfigIsSafe(t *testing.T) {
	a, b := defaultConfig(), defaultConfig()
	if a.Password == b.Password {
		t.Fatal("两次生成的默认密码一样 —— 密码不是随机的")
	}
	if len(a.Password) < 20 {
		t.Errorf("默认密码只有 %d 位，太短", len(a.Password))
	}
	if a.AllowLAN {
		t.Error("默认不该允许局域网直连")
	}
	if !validPolicy(a.MaxMemPolicy) {
		t.Errorf("默认淘汰策略 %q 不合法", a.MaxMemPolicy)
	}
	// 抄写用的密码里不该有容易看错的字符
	for _, c := range "0O1lI" {
		if strings.ContainsRune(a.Password, c) {
			t.Errorf("默认密码里出现了容易看错的字符 %q", c)
		}
	}
}
