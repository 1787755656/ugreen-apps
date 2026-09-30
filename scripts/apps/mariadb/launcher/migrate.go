package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// 迁移数据目录。
//
// 早先这里什么都不做，只在管理页上提示一句"搬迁需要停服后手动复制，应用不会自动搬"。
// 那是不对的：应用本来就有停服的能力、两个路径也都知道，让用户去 SSH 里 cp -a
// 没有任何道理（用户原话："这完全没道理啊，就是个移动文件的问题"）。
//
// 当初"绝不自动搬"的顾虑其实只针对【每次启动都跟着环境变量漂移】——
// 首启参数为空会让数据库落在 A、重启后落在 B，用户看到的是"数据突然没了"。
// 这个顾虑和"提供一个显式的迁移动作"并不冲突：
//
//	启动路径：只认配置里的 DataDir，永远不跟着参数漂 —— 保持原样，不改。
//	迁移动作：由人在管理页上点一次，复制 → 校验 → 切配置 → 重启。
//
// 安全性靠顺序保证：**校验通过之前绝不切配置，切配置成功之前绝不动旧数据。**
// 中途任何一步失败（断电、空间不够、复制出错），配置都还指着旧目录，
// 最坏结果只是新目录里留下一份没用完的副本。

// migrationSubdir 是数据落在用户所选文件夹下的哪个子目录。
// 和 ResolveDataDir 首次初始化时的选择保持一致，免得迁移前后规则不一样。
const migrationSubdir = "mariadb-data"

// TargetFromParam 把安装参数里的文件夹换算成真正的数据目录。
func TargetFromParam(param string) string {
	return filepath.Join(param, migrationSubdir)
}

// checkMigrationTarget 在动手之前把能查的都查了。
// 抽成纯函数是为了能直接对着临时目录测——真机上的失败场景很难造。
func checkMigrationTarget(oldDir, newDir string) error {
	if strings.TrimSpace(newDir) == "" {
		return fmt.Errorf("没有指定目标目录")
	}
	if !filepath.IsAbs(newDir) {
		return fmt.Errorf("目标目录必须是绝对路径：%s", newDir)
	}
	oldClean := filepath.Clean(oldDir)
	newClean := filepath.Clean(newDir)
	if oldClean == newClean {
		return fmt.Errorf("目标目录和当前目录是同一个（%s），不用迁移", newClean)
	}
	// 嵌套会出大事：目标在源里面 → 边复制边把复制出来的东西再复制一遍；
	// 源在目标里面 → 之后清理旧目录会把新数据一起端掉。
	if isSubPath(oldClean, newClean) {
		return fmt.Errorf("目标目录 %s 在当前数据目录里面，不能这样迁移", newClean)
	}
	if isSubPath(newClean, oldClean) {
		return fmt.Errorf("当前数据目录 %s 在目标目录里面，不能这样迁移", oldClean)
	}

	// 目标要么不存在，要么是个空目录。里面已经有数据库的话绝不覆盖。
	if fi, err := os.Stat(newClean); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("目标 %s 已存在且不是目录", newClean)
		}
		entries, err := os.ReadDir(newClean)
		if err != nil {
			return fmt.Errorf("读取目标目录失败: %w", err)
		}
		if len(entries) > 0 {
			if _, err := os.Stat(filepath.Join(newClean, "mysql")); err == nil {
				return fmt.Errorf("目标目录 %s 里已经有一个数据库了，"+
					"不会覆盖它。请换一个空目录", newClean)
			}
			return fmt.Errorf("目标目录 %s 不是空的（%d 个条目）。"+
				"为安全起见只往空目录迁移", newClean, len(entries))
		}
	}
	return nil
}

// isSubPath 判断 child 是不是在 parent 里面（含相等的情况由调用方另判）。
// 按【路径分段】比，不能用字符串前缀 —— /volume1/ab 会被 /volume1/a 命中。
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

// dirSize 统计目录里普通文件的总字节数和文件数，用于迁移前估空间、迁移后核对。
func dirSize(root string) (bytes int64, files int64, err error) {
	err = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			bytes += fi.Size()
			files++
		}
		return nil
	})
	return
}

// freeSpace 返回路径所在文件系统的可用字节数。
// Bsize 在 linux 上是 int64、在 darwin 上是 uint32，统一转成 uint64 两边都能编。
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// copyTree 递归复制。保留权限位；遇到符号链接或设备文件【直接报错】而不是跳过 ——
// 数据目录里本来就不该有这些，静默跳过会复制出一个看起来成功、实际残缺的副本。
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case fi.IsDir():
			return os.MkdirAll(target, fi.Mode().Perm())
		case fi.Mode().IsRegular():
			return copyFile(p, target, fi.Mode().Perm())
		default:
			return fmt.Errorf("数据目录里有无法复制的条目 %s（类型 %s）。"+
				"请先处理掉它再迁移", p, fi.Mode().Type())
		}
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	// 必须显式 Sync 再 Close：迁移完马上就要停旧的、起新的，
	// 数据还在页缓存里没落盘的话，这期间掉电就等于数据没搬过去。
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// MigrateDataDir 把数据目录整体搬到 newDir，然后让数据库在新位置起来。
//
// 顺序是刻意的，每一步失败都能安全退回：
//
//	1 校验目标（空间、嵌套、非空）   ← 失败：什么都没动
//	2 停服（WithStopped 负责）
//	3 复制                          ← 失败：配置仍指旧目录，新目录留下半份副本
//	4 核对文件数与字节数              ← 失败：同上
//	5 切配置 + 切内存里的路径          ← 过了这一步才算迁移成功
//	6 把旧目录改名成 .migrated-<时间>   ← 失败只记日志，不影响结果
//	7 在新位置启动（WithStopped 负责）
func (s *Server) MigrateDataDir(newDir string, onPhase func(string)) error {
	if onPhase == nil {
		onPhase = func(string) {}
	}
	oldDir := s.DataDir()
	newDir = filepath.Clean(newDir)

	if err := checkMigrationTarget(oldDir, newDir); err != nil {
		return err
	}
	// 这次统计【只用来估空间】，不能拿去做校验 —— 此刻数据库还在跑。
	// 见下面 fn() 里那段注释。
	estBytes, _, err := dirSize(oldDir)
	if err != nil {
		return fmt.Errorf("统计当前数据目录失败: %w", err)
	}
	if err := os.MkdirAll(newDir, 0o750); err != nil {
		return fmt.Errorf("创建目标目录 %s 失败: %w（如果它在共享文件夹里，"+
			"确认安装参数已经选中该目录、并且应用重启过一次）", newDir, err)
	}
	// 留 5% 余量：复制过程中数据库是停的，但同一个卷上可能还有别的应用在写。
	if avail, err := freeSpace(newDir); err == nil {
		need := uint64(float64(estBytes) * 1.05)
		if avail < need {
			return fmt.Errorf("目标所在磁盘可用空间不足：需要约 %s，只剩 %s",
				humanBytes(int64(need)), humanBytes(int64(avail)))
		}
	}

	logf("准备迁移数据目录：%s → %s（约 %s）", oldDir, newDir, humanBytes(estBytes))
	onPhase("正在停止数据库…")

	return s.WithStopped(func() error {
		// ⚠ 源大小必须在【停库之后】重新量一次，绝不能用停库前那份。
		//
		// 2026-08-09 真机上就栽在这儿：停库前量到 211 个文件 / 154.5 MiB，
		// 而 mariadbd 干净关闭时会删掉 ibtmp1（12 MiB 的临时表空间）等文件，
		// 复制出来只有 208 个文件 / 142.5 MiB。于是校验判定"复制结果和原数据
		// 对不上"，整个迁移被放弃 —— 其实一个字节都没错，是【基准取错了时刻】。
		srcBytes, srcFiles, err := dirSize(oldDir)
		if err != nil {
			return fmt.Errorf("统计当前数据目录失败: %w", err)
		}
		logf("数据库已停止，开始复制：%d 个文件 / %s", srcFiles, humanBytes(srcBytes))
		onPhase(fmt.Sprintf("数据库已停止，正在复制 %d 个文件 / %s…", srcFiles, humanBytes(srcBytes)))

		if err := copyTree(oldDir, newDir); err != nil {
			return fmt.Errorf("复制数据失败: %w（原数据没有被动过，"+
				"数据库仍会在原位置启动；请清理 %s 后重试）", err, newDir)
		}
		onPhase("复制完成，正在核对文件数与字节数…")
		dstBytes, dstFiles, err := dirSize(newDir)
		if err != nil {
			return fmt.Errorf("核对复制结果失败: %w", err)
		}
		if dstBytes != srcBytes || dstFiles != srcFiles {
			return fmt.Errorf("复制结果和原数据对不上（原 %d 文件/%d 字节，"+
				"新 %d 文件/%d 字节），已放弃迁移，数据库仍在原位置",
				srcFiles, srcBytes, dstFiles, dstBytes)
		}
		logf("复制完毕并核对一致：%d 个文件 / %s", dstFiles, humanBytes(dstBytes))

		// 到这里才算成功。先落配置，再改内存里的路径。
		if err := s.cfg.Update(func(d *ConfigData) error {
			d.DataDir = newDir
			// 同步这个值，管理页上"参数改了但数据没搬"的提示才会消失。
			d.DataDirParamAtInit = paramDataDir()
			return nil
		}); err != nil {
			return fmt.Errorf("保存新的数据目录失败: %w（数据已经复制到 %s，"+
				"但配置没更新，数据库仍会在原位置启动）", err, newDir)
		}
		s.setDataDir(newDir)

		// 旧目录改名而不是删除：迁移刚完成就把几个 G 的原始数据删掉太激进了。
		// 改名能防止"配置万一被重置后又从旧目录启动"，同时留一条后悔路。
		retired := oldDir + ".migrated-" + time.Now().Format("20060102-150405")
		if err := os.Rename(oldDir, retired); err != nil {
			logf("::warning:: 旧数据目录改名失败（%v）。迁移本身已成功，"+
				"数据库会在新位置启动；旧数据仍在 %s，确认无误后可以手动删除", err, oldDir)
		} else {
			logf("迁移完成。旧数据已改名为 %s —— 确认新位置一切正常后可以手动删除它", retired)
		}

		// 迁移成功后【一定要】把数据库拉起来，哪怕迁移之前它是停着的。
		//
		// WithStopped 默认"迁移前停着的、迁移后就还停着"，那对改密码之类的操作是对的，
		// 但迁移不一样：用户点这个按钮的意思就是"把数据搬过去然后继续用"。
		// 真机上就出现过——用户迁移前手动停过一次库，迁移成功后数据库不自动起来，
		// 还得自己再点一次「启动」（用户原话：应该检测文件传输完毕再自动启动）。
		s.mu.Lock()
		s.desired = true
		s.mu.Unlock()
		logf("准备在新位置启动数据库…")
		onPhase("核对通过，正在新位置启动数据库…")
		return nil
	})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
