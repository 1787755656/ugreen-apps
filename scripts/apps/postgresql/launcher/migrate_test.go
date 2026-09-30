package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// makeReadyPGData 造一个"看起来初始化完整"的 PGDATA。
func makeReadyPGData(t *testing.T, dir string) {
	t.Helper()
	mkFile(t, filepath.Join(dir, "PG_VERSION"), "17\n", 0o600)
	mkFile(t, filepath.Join(dir, initDoneMarker), "ok\n", 0o600)
	mkFile(t, filepath.Join(dir, "postgresql.conf"), "port = 5432\n", 0o600)
	mkFile(t, filepath.Join(dir, "global", "pg_control"), "x", 0o600)
}

// 目标校验必须在【动手之前】就把能拒的都拒掉 —— 复制到一半才失败的话，
// 用户已经白等了一次停库，目标目录里还留着半份垃圾。
func TestCheckMigrationTarget(t *testing.T) {
	base := t.TempDir()
	oldPGData := filepath.Join(base, "old", pgDataSubdir)
	makeReadyPGData(t, oldPGData)

	t.Run("目标算出来就是当前位置", func(t *testing.T) {
		if err := checkMigrationTarget(oldPGData, filepath.Join(base, "old")); err == nil {
			t.Fatal("目标落点算出来和当前 PGDATA 一样，应当拒绝")
		}
	})

	t.Run("相对路径", func(t *testing.T) {
		if err := checkMigrationTarget(oldPGData, "relative/path"); err == nil {
			t.Fatal("相对路径应当拒绝")
		}
	})

	// 嵌套是最危险的一种：目标在源里面会边复制边把复制出来的再复制一遍；
	// 源在目标里面则会在给旧目录改名时把新数据一起端走。
	t.Run("目标在源里面", func(t *testing.T) {
		err := checkMigrationTarget(oldPGData, filepath.Join(oldPGData, "sub"))
		if err == nil || !strings.Contains(err.Error(), "在当前数据目录里面") {
			t.Fatalf("应当因嵌套被拒，实际 %v", err)
		}
	})

	t.Run("源在目标里面", func(t *testing.T) {
		err := checkMigrationTarget(oldPGData, filepath.Join(base, "old"))
		// 这个 case 里目标落点就是源的上一级，先被"算出来是同一个"拦住也可以，
		// 只要是拒绝就行 —— 它绝不能放行。
		if err == nil {
			t.Fatal("目标是源的上一级，应当拒绝")
		}
	})

	// 路径前缀陷阱：/x/ab 不是 /x/a 的子目录，不能用字符串前缀判断。
	t.Run("同前缀但不是子目录_应当放行", func(t *testing.T) {
		sibling := filepath.Join(base, "old-backup")
		if err := checkMigrationTarget(oldPGData, sibling); err != nil {
			t.Fatalf("%s 不是 %s 的子目录，应当放行，实际 %v", sibling, oldPGData, err)
		}
	})

	t.Run("目标里已经有完好的数据库", func(t *testing.T) {
		occupied := filepath.Join(base, "occupied")
		makeReadyPGData(t, filepath.Join(occupied, pgDataSubdir))
		err := checkMigrationTarget(oldPGData, occupied)
		if err == nil || !strings.Contains(err.Error(), "完好的数据库") {
			t.Fatalf("绝不能覆盖已有数据库，实际 %v", err)
		}
	})

	t.Run("目标非空", func(t *testing.T) {
		dirty := filepath.Join(base, "dirty")
		mkFile(t, filepath.Join(dirty, "readme.txt"), "x", 0o600)
		err := checkMigrationTarget(oldPGData, dirty)
		if err == nil || !strings.Contains(err.Error(), "不是空的") {
			t.Fatalf("非空目录应当拒绝，实际 %v", err)
		}
	})

	t.Run("空目录和不存在的目录都放行", func(t *testing.T) {
		empty := filepath.Join(base, "empty")
		if err := os.MkdirAll(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := checkMigrationTarget(oldPGData, empty); err != nil {
			t.Fatalf("空目录应当放行，实际 %v", err)
		}
		if err := checkMigrationTarget(oldPGData, filepath.Join(base, "nope")); err != nil {
			t.Fatalf("不存在的目录应当放行（会被创建），实际 %v", err)
		}
	})
}

func TestIsSubPathDoesNotUseStringPrefix(t *testing.T) {
	if isSubPath("/volume1/a", "/volume1/ab") {
		t.Fatal("/volume1/ab 不是 /volume1/a 的子目录 —— 不能用字符串前缀判断")
	}
	if !isSubPath("/volume1/a", "/volume1/a/b") {
		t.Fatal("/volume1/a/b 应当算 /volume1/a 的子目录")
	}
	if isSubPath("/volume1/a", "/volume1/a") {
		t.Fatal("相等不算子目录（由调用方单独判）")
	}
}

// clearMigrationTarget 是全应用唯一一处会动"别人的目录"的代码，
// 它必须打死也不碰一个完好的数据库。
func TestClearMigrationTargetRefusesRealDatabase(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	makeReadyPGData(t, filepath.Join(target, pgDataSubdir))

	if _, err := clearMigrationTarget(target); err == nil {
		t.Fatal("目标里有完好的数据库，必须拒绝清理")
	}
	// 而且原样还在
	if _, err := os.Stat(filepath.Join(target, pgDataSubdir, "PG_VERSION")); err != nil {
		t.Fatalf("被拒绝之后目标目录不该有任何改动: %v", err)
	}
}

func TestClearMigrationTargetRenamesRatherThanDeletes(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	mkFile(t, filepath.Join(target, "junk.txt"), "half a copy", 0o600)

	retired, err := clearMigrationTarget(target)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	// 【改名不是删除】：判据万一错了，删掉就再也回不来。
	if _, err := os.Stat(filepath.Join(retired, "junk.txt")); err != nil {
		t.Fatalf("原有内容应当被改名保留，而不是删除: %v", err)
	}
}

// 复制必须一字节不差，权限位也要跟着走 —— PGDATA 的权限不对，
// postgres 会拒绝启动（它要求 0700 或 0750）。
func TestCopyTreePreservesContentAndPerms(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")

	mkFile(t, filepath.Join(src, "PG_VERSION"), "17\n", 0o600)
	mkFile(t, filepath.Join(src, "global", "pg_control"), "bbbbbb", 0o600)
	mkFile(t, filepath.Join(src, "base", "1", "1259"), "cc", 0o600)
	if err := os.MkdirAll(filepath.Join(src, "pg_wal", "archive_status"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	srcBytes, srcFiles, err := dirSize(src)
	if err != nil {
		t.Fatal(err)
	}
	dstBytes, dstFiles, err := dirSize(dst)
	if err != nil {
		t.Fatal(err)
	}
	if srcBytes != dstBytes || srcFiles != dstFiles {
		t.Fatalf("复制前后对不上：原 %d 文件/%d 字节，新 %d 文件/%d 字节",
			srcFiles, srcBytes, dstFiles, dstBytes)
	}

	for _, rel := range []string{"PG_VERSION", "global/pg_control", "base/1/1259"} {
		want, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("目标缺少 %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s 内容不一致", rel)
		}
		si, _ := os.Stat(filepath.Join(src, filepath.FromSlash(rel)))
		di, _ := os.Stat(filepath.Join(dst, filepath.FromSlash(rel)))
		if si.Mode().Perm() != di.Mode().Perm() {
			t.Errorf("%s 权限位没跟着走：原 %v，新 %v", rel, si.Mode().Perm(), di.Mode().Perm())
		}
	}
	// 空目录也要建出来，别只复制有文件的那些
	if fi, err := os.Stat(filepath.Join(dst, "pg_wal", "archive_status")); err != nil || !fi.IsDir() {
		t.Errorf("空子目录没有被复制过去: %v", err)
	}
}

// 遇到符号链接要【报错】而不是静默跳过。
// PGDATA 里出现软链基本只有一种情况：表空间 —— 那意味着还有一份数据在别处，
// 闷头跳过会复制出一个"看着成功、实际残缺"的数据库。
func TestCopyTreeRefusesSymlinks(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkFile(t, filepath.Join(src, "PG_VERSION"), "17\n", 0o600)
	if err := os.MkdirAll(filepath.Join(src, "pg_tblspc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "PG_VERSION"),
		filepath.Join(src, "pg_tblspc", "16385")); err != nil {
		t.Skipf("这个文件系统建不了符号链接: %v", err)
	}
	err := copyTree(src, filepath.Join(base, "dst"))
	if err == nil || !strings.Contains(err.Error(), "无法复制") {
		t.Fatalf("应当因为符号链接报错，实际 %v", err)
	}
}

// 迁移的落点必须和首次初始化时的规则一致，否则同一个参数值在"新装"和"迁移"
// 两条路上会指到不同的地方，用户完全看不懂。
func TestPGBaseFromParamMatchesFirstInitLayout(t *testing.T) {
	got := PGBaseFromParam("/volume1/test/testapp")
	want := filepath.Join("/volume1/test/testapp", pgBaseSubdir)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// begin() 曾经写成 *m = migrateState{...}，那会把正持有的锁一起覆盖掉，
// 紧接着的 Unlock 解的是另一把从没锁过的锁 —— 运行时直接 panic。
func TestMigrateStateBeginDoesNotClobberItsOwnLock(t *testing.T) {
	var m migrateState
	if !m.begin("/a", "/b") {
		t.Fatal("第一次 begin 应当成功")
	}
	if m.begin("/a", "/c") {
		t.Fatal("已经在迁移中，第二次 begin 必须失败")
	}
	if s := m.Snapshot(); !s.Running || s.To != "/b" {
		t.Fatalf("状态不对: %+v", s)
	}
	m.finish(nil, "done")
	if s := m.Snapshot(); s.Running || !s.Finished || s.Result != "done" {
		t.Fatalf("finish 之后状态不对: %+v", s)
	}
	// 收尾之后必须能再开一次，否则失败一次就永远不能重试了
	if !m.begin("/a", "/d") {
		t.Fatal("上一次已经结束，应当能再次 begin")
	}
}
