package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// 目标校验必须在【动手之前】就把能拒的都拒掉 —— 复制到一半才失败的话，
// 用户已经白等了一次停库，目标目录里还留着半份垃圾。
func TestCheckMigrationTarget(t *testing.T) {
	base := t.TempDir()
	old := filepath.Join(base, "old")
	mkFile(t, filepath.Join(old, "mysql", "global_priv.frm"), "x", 0o640)

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
	// 源在目标里面则会在清理旧目录时把新数据一起端掉。
	t.Run("目标在源里面", func(t *testing.T) {
		err := checkMigrationTarget(old, filepath.Join(old, "sub", "data"))
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

	t.Run("目标里已经有数据库", func(t *testing.T) {
		occupied := filepath.Join(base, "occupied")
		mkFile(t, filepath.Join(occupied, "mysql", "user.frm"), "x", 0o640)
		err := checkMigrationTarget(old, occupied)
		if err == nil || !strings.Contains(err.Error(), "已经有一个数据库") {
			t.Fatalf("绝不能覆盖已有数据库，实际 %v", err)
		}
	})

	t.Run("目标非空", func(t *testing.T) {
		dirty := filepath.Join(base, "dirty")
		mkFile(t, filepath.Join(dirty, "readme.txt"), "x", 0o640)
		err := checkMigrationTarget(old, dirty)
		if err == nil || !strings.Contains(err.Error(), "不是空的") {
			t.Fatalf("非空目录应当拒绝，实际 %v", err)
		}
	})

	t.Run("空目录和不存在的目录都放行", func(t *testing.T) {
		empty := filepath.Join(base, "empty")
		if err := os.MkdirAll(empty, 0o750); err != nil {
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

// 复制必须一字节不差，权限位也要跟着走 —— InnoDB 的文件权限不对，
// 数据库在新位置会起不来。
func TestCopyTreePreservesContentAndPerms(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	dst := filepath.Join(base, "dst")

	mkFile(t, filepath.Join(src, "ibdata1"), "aaaa", 0o640)
	mkFile(t, filepath.Join(src, "mysql", "global_priv.frm"), "bbbbbb", 0o600)
	mkFile(t, filepath.Join(src, "mysql", "user.frm"), "cc", 0o640)
	if err := os.MkdirAll(filepath.Join(src, "performance_schema"), 0o750); err != nil {
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

	for _, rel := range []string{"ibdata1", "mysql/global_priv.frm", "mysql/user.frm"} {
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
	if fi, err := os.Stat(filepath.Join(dst, "performance_schema")); err != nil || !fi.IsDir() {
		t.Errorf("空子目录没有被复制过去: %v", err)
	}
}

// 遇到符号链接/设备文件要【报错】而不是静默跳过 ——
// 跳过会复制出一个看着成功、实际残缺的副本，那比失败糟糕得多。
func TestCopyTreeRefusesNonRegularFiles(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkFile(t, filepath.Join(src, "ibdata1"), "x", 0o640)
	if err := os.Symlink(filepath.Join(src, "ibdata1"), filepath.Join(src, "link")); err != nil {
		t.Skipf("这个文件系统建不了符号链接: %v", err)
	}
	err := copyTree(src, filepath.Join(base, "dst"))
	if err == nil || !strings.Contains(err.Error(), "无法复制") {
		t.Fatalf("应当因为符号链接报错，实际 %v", err)
	}
}

func TestTargetFromParamMatchesFirstInitLayout(t *testing.T) {
	// 迁移的落点必须和 ResolveDataDir 首次初始化时的选择一致，
	// 否则同一个参数在"首次安装"和"迁移"两条路上会指到不同地方。
	if got, want := TargetFromParam("/volume1/test/testapp"),
		filepath.Join("/volume1/test/testapp", "mariadb-data"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 迁移成功后必须把数据库拉起来，【哪怕迁移之前它是停着的】。
//
// WithStopped 默认"操作前停着的、操作后还停着"，那对改密码是对的；
// 但用户点「迁移」的意思就是"搬完继续用"。真机上出现过迁移成功却不自动启动、
// 用户还得再点一次「启动」的情况，这条测试守的就是那个。
func TestWithStoppedHonoursDesiredRaisedInsideFn(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 路径都指向不存在的文件：startLocked 会走到 exec 然后失败返回错误，
	// 不会真的 fork 出一个 mariadbd，也不会 panic。我们只关心它【走没走到那一步】。
	s := NewServer(&Paths{
		Data: dir, Cache: dir, Log: dir,
		Base: dir, LibDir: dir, ShareDir: dir,
		Loader: filepath.Join(dir, "no-such-loader"),
	}, cfg, filepath.Join(dir, "data"))
	s.desired = false // 模拟"用户在迁移前手动停过库"

	ran := false
	_ = s.WithStopped(func() error {
		s.mu.Lock()
		s.desired = true // 迁移成功时做的事
		s.mu.Unlock()
		ran = true
		return nil
	})
	if !ran {
		t.Fatal("fn 没有被执行")
	}
	// 关键：WithStopped 开头拍的那张 desired=false 的快照不能把 fn 的意图盖掉。
	// 盖掉的表现就是真机上那次——迁移成功了，数据库却不自动启动。
	s.mu.Lock()
	got := s.desired
	s.mu.Unlock()
	if !got {
		t.Fatal("fn 里把 desired 置 true 被忽略了，迁移完数据库不会自动启动")
	}
}
