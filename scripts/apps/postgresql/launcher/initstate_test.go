package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 这组测试守的是一类真实事故：initdb 跑到一半被打断（系统重启、OOM、用户点了停止），
// 留下一个"有 PG_VERSION 但根本不能用"的目录。旧逻辑只看 PG_VERSION 在不在，
// 会把它判成"已初始化"，此后每次启动都是 postgres 起来就崩，
// 而页面上只有一句"意外退出" —— 用户永远不知道该怎么办。
func TestClassifyPGData(t *testing.T) {
	base := t.TempDir()

	t.Run("目录不存在_是全新的", func(t *testing.T) {
		if st, _ := classifyPGData(filepath.Join(base, "nope")); st != InitFresh {
			t.Fatalf("want InitFresh, got %v", st)
		}
	})

	t.Run("空目录_是全新的", func(t *testing.T) {
		d := filepath.Join(base, "empty")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if st, _ := classifyPGData(d); st != InitFresh {
			t.Fatalf("want InitFresh, got %v", st)
		}
	})

	t.Run("有完成标记_可以用", func(t *testing.T) {
		d := filepath.Join(base, "ready")
		mkFile(t, filepath.Join(d, "PG_VERSION"), "17\n", 0o600)
		mkFile(t, filepath.Join(d, initDoneMarker), "ok\n", 0o600)
		if st, _ := classifyPGData(d); st != InitReady {
			t.Fatalf("want InitReady, got %v", st)
		}
	})

	// 老版本的应用不写完成标记。判据是 postgresql.conf 末尾那两行 include ——
	// 它只在 initdb 成功【返回之后】才被追加，半截的 initdb 产物里不会有。
	// 判错的代价是把老用户的数据库当成损坏的，那绝对不能发生。
	t.Run("老版本装的_没标记但配置里有我们追加的include", func(t *testing.T) {
		d := filepath.Join(base, "legacy")
		mkFile(t, filepath.Join(d, "PG_VERSION"), "17\n", 0o600)
		mkFile(t, filepath.Join(d, "postgresql.conf"),
			"port = 5432\ninclude_if_exists = 'postgresql.ugos.conf'\n", 0o600)
		st, why := classifyPGData(d)
		if st != InitReady {
			t.Fatalf("老版本装的数据库必须被认出来，got %v（%s）", st, why)
		}
	})

	t.Run("initdb半截_有PG_VERSION但没别的痕迹", func(t *testing.T) {
		d := filepath.Join(base, "half")
		mkFile(t, filepath.Join(d, "PG_VERSION"), "17\n", 0o600)
		// initdb 早期就写出来的原始 postgresql.conf，没有我们追加的 include
		mkFile(t, filepath.Join(d, "postgresql.conf"), "#port = 5432\n", 0o600)
		st, _ := classifyPGData(d)
		if st != InitBroken {
			t.Fatalf("want InitBroken, got %v", st)
		}
	})

	t.Run("目录里是别的东西", func(t *testing.T) {
		d := filepath.Join(base, "junk")
		mkFile(t, filepath.Join(d, "我的照片.jpg"), "x", 0o600)
		st, _ := classifyPGData(d)
		if st != InitBroken {
			t.Fatalf("非空又不是数据库，必须判成 InitBroken 而不是 InitFresh —— "+
				"否则会往用户的文件夹里 initdb，got %v", st)
		}
	})
}

func TestMarkInitDoneMakesDirReady(t *testing.T) {
	d := filepath.Join(t.TempDir(), "pgdata")
	mkFile(t, filepath.Join(d, "PG_VERSION"), "17\n", 0o600)
	mkFile(t, filepath.Join(d, "postgresql.conf"), "#port = 5432\n", 0o600)

	if st, _ := classifyPGData(d); st != InitBroken {
		t.Fatalf("打标记之前应当是 InitBroken，got %v", st)
	}
	if err := markInitDone(d); err != nil {
		t.Fatal(err)
	}
	if st, _ := classifyPGData(d); st != InitReady {
		t.Fatalf("打完标记应当是 InitReady，got %v", st)
	}
	// 标记必须是私有的：它和数据一起躺在用户的共享文件夹里
	fi, err := os.Stat(filepath.Join(d, initDoneMarker))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("完成标记权限应当是 0600，got %v", fi.Mode().Perm())
	}
}
