package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// 目标校验必须在【动手之前】就把能拒的都拒掉 —— 复制到一半才失败的话，
// 用户已经白等了一次停服，目标目录里还留着半份垃圾。
func TestCheckMigrationTarget(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	mkFile(t, filepath.Join(old, "dump.rdb"), "REDIS0011", 0o600)

	t.Run("同一个目录", func(t *testing.T) {
		if err := checkMigrationTarget(old, old); err == nil {
			t.Fatal("目标和当前是同一个，应当拒绝")
		}
	})

	t.Run("相对路径", func(t *testing.T) {
		if err := checkMigrationTarget(old, "relative/path"); err == nil {
			t.Fatal("相对路径应当拒绝")
		}
	})

	// 嵌套是最危险的一种：目标在源里面会边复制边把复制出来的再复制一遍；
	// 源在目标里面则会在给旧目录改名时把新数据一起端走。
	t.Run("目标在源里面", func(t *testing.T) {
		err := checkMigrationTarget(old, filepath.Join(old, "sub"))
		if err == nil || !strings.Contains(err.Error(), "在当前数据目录里面") {
			t.Fatalf("应当因嵌套被拒，实际 %v", err)
		}
	})

	t.Run("源在目标里面", func(t *testing.T) {
		err := checkMigrationTarget(old, base)
		if err == nil || !strings.Contains(err.Error(), "在目标目录里面") {
			t.Fatalf("应当因嵌套被拒，实际 %v", err)
		}
	})

	// 路径前缀陷阱：/x/ab 不是 /x/a 的子目录，不能用字符串前缀判断。
	t.Run("同前缀但不是子目录_应当放行", func(t *testing.T) {
		sibling := filepath.Join(base, "old-backup")
		if err := checkMigrationTarget(old, sibling); err != nil {
			t.Fatalf("%s 不是 %s 的子目录，应当放行，实际 %v", sibling, old, err)
		}
	})

	t.Run("目标里已经有数据", func(t *testing.T) {
		for _, marker := range []string{"dump.rdb", "appendonlydir/x.manifest"} {
			occupied := filepath.Join(base, "occupied-"+strings.ReplaceAll(marker, "/", "-"))
			mkFile(t, filepath.Join(occupied, marker), "x", 0o600)
			err := checkMigrationTarget(old, occupied)
			if err == nil || !strings.Contains(err.Error(), "已经有 Redis 的数据文件") {
				t.Fatalf("有 %s 时绝不能覆盖，实际 %v", marker, err)
			}
		}
	})

	t.Run("目标非空", func(t *testing.T) {
		dirty := filepath.Join(base, "dirty")
		mkFile(t, filepath.Join(dirty, "readme.txt"), "x", 0o600)
		err := checkMigrationTarget(old, dirty)
		if err == nil || !strings.Contains(err.Error(), "不是空的") {
			t.Fatalf("非空目录应当拒绝，实际 %v", err)
		}
	})

	t.Run("空目录和不存在的目录都放行", func(t *testing.T) {
		empty := filepath.Join(base, "empty")
		if err := os.MkdirAll(empty, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := checkMigrationTarget(old, empty); err != nil {
			t.Fatalf("空目录应当放行，实际 %v", err)
		}
		if err := checkMigrationTarget(old, filepath.Join(base, "nope")); err != nil {
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
// 它必须打死也不碰一个有数据的目录，而且【只改名不删除】。
func TestClearMigrationTarget(t *testing.T) {
	base := t.TempDir()

	withData := filepath.Join(base, "withdata")
	mkFile(t, filepath.Join(withData, "dump.rdb"), "x", 0o600)
	if _, err := clearMigrationTarget(withData); err == nil {
		t.Fatal("目标里有 Redis 数据，必须拒绝清理")
	}
	if _, err := os.Stat(filepath.Join(withData, "dump.rdb")); err != nil {
		t.Fatalf("被拒绝之后目标目录不该有任何改动: %v", err)
	}

	junk := filepath.Join(base, "junk")
	mkFile(t, filepath.Join(junk, "half.txt"), "half a copy", 0o600)
	retired, err := clearMigrationTarget(junk)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(retired, "half.txt")); err != nil {
		t.Fatalf("原有内容应当被改名保留，而不是删除: %v", err)
	}
}

// 复制必须一字节不差，权限位也要跟着走。
func TestCopyTreePreservesContentAndPerms(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")

	mkFile(t, filepath.Join(src, "dump.rdb"), "REDIS0011payload", 0o600)
	mkFile(t, filepath.Join(src, "appendonlydir", "appendonly.aof.1.base.rdb"), "bbbb", 0o600)
	mkFile(t, filepath.Join(src, "appendonlydir", "appendonly.aof.manifest"), "cc", 0o644)
	if err := os.MkdirAll(filepath.Join(src, "empty"), 0o700); err != nil {
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

	for _, rel := range []string{"dump.rdb", "appendonlydir/appendonly.aof.manifest"} {
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
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("空子目录没有被复制过去: %v", err)
	}
}

// 遇到符号链接要【报错】而不是静默跳过 ——
// 跳过会复制出一个看着成功、实际残缺的副本，那比明确失败糟糕得多。
func TestCopyTreeRefusesSymlinks(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkFile(t, filepath.Join(src, "dump.rdb"), "x", 0o600)
	if err := os.Symlink(filepath.Join(src, "dump.rdb"), filepath.Join(src, "link")); err != nil {
		t.Skipf("这个文件系统建不了符号链接: %v", err)
	}
	err := copyTree(src, filepath.Join(base, "dst"))
	if err == nil || !strings.Contains(err.Error(), "无法复制") {
		t.Fatalf("应当因为符号链接报错，实际 %v", err)
	}
}

// 迁移的落点必须和首次初始化时的规则一致，否则同一个参数值在"新装"和"迁移"
// 两条路上会指到不同的地方，用户完全看不懂。
func TestDataBaseFromParam(t *testing.T) {
	got := DataBaseFromParam("/volume1/test/testapp")
	want := filepath.Join("/volume1/test/testapp", dataSubdir)
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

// 真机上报回来的 bug：数据早就搬完了，管理页上那张「迁移数据目录」的卡片
// 却一直不消失 —— 因为 finished 一旦置上就再也不清，而页面在 finished 时
// 也会显示卡片（要让用户看见结果）。
func TestMigrateResultGoesAway(t *testing.T) {
	t.Run("点了知道了就收起来", func(t *testing.T) {
		var m migrateState
		m.begin("/a", "/b")
		m.finish(nil, "done")
		if s := m.Snapshot(); !s.Finished {
			t.Fatal("刚结束时应当还看得见结果")
		}
		m.Dismiss()
		if s := m.Snapshot(); s.Finished || s.Result != "" || s.Phase != "" {
			t.Fatalf("收起来之后不该再报任何东西: %+v", s)
		}
	})

	t.Run("放太久自动过期", func(t *testing.T) {
		var m migrateState
		m.begin("/a", "/b")
		m.finish(nil, "done")
		// 假装是很久以前结束的
		m.mu.Lock()
		m.finished = time.Now().Add(-migrateResultTTL - time.Minute)
		m.mu.Unlock()
		if s := m.Snapshot(); s.Finished {
			t.Fatal("超过 TTL 之后卡片就该自己消失，不能永远挂着")
		}
	})

	t.Run("正在跑的时候收不起来", func(t *testing.T) {
		var m migrateState
		m.begin("/a", "/b")
		m.Dismiss()
		if s := m.Snapshot(); !s.Running {
			t.Fatal("迁移还在跑，进度不能被 Dismiss 抹掉")
		}
	})

	t.Run("新的一次迁移会重新显示", func(t *testing.T) {
		var m migrateState
		m.begin("/a", "/b")
		m.finish(nil, "done")
		m.Dismiss()
		m.begin("/a", "/c")
		if s := m.Snapshot(); !s.Running || s.To != "/c" {
			t.Fatalf("上次的「知道了」不该把新的一次也盖住: %+v", s)
		}
	})
}
