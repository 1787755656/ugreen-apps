package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 把 Redis 的数据目录（dump.rdb / appendonlydir）搬到别处。
//
// 为什么应用必须自己做这件事：安装参数里的"数据保存目录"改了之后，数据不会
// 自己跟过去 —— 让用户去 SSH 里 cp -a 是说不通的，NAS 用户手上通常根本没有 SSH，
// 而应用本来就有停服的能力、两个路径也都知道。
//
// 安全性靠【顺序】保证，而不是靠事务：
//
//	1 校验目标（绝对路径、不嵌套、空的、空间够）  ← 失败：什么都没动
//	2 停服（SIGTERM，Redis 会先存一次盘）
//	3 停服【之后】才量源大小                      ← 见下面 srcBytes 那段注释
//	4 复制                                        ← 失败：配置仍指旧目录，照常在原位启动
//	5 核对文件数与字节数                          ← 同上
//	6 切配置 + 切内存里的路径                     ← 过了这一步才算成功
//	7 旧目录改名成 .migrated-<时间>（不删）
//	8 在新位置启动
//
// 也就是说：**校验通过之前绝不切配置，切配置成功之前绝不动旧数据。**

// ---- 进度状态 ------------------------------------------------------------

// migrateState 是给管理页轮询看的迁移进度。
//
// 迁移可能要跑几分钟，HTTP 请求不能一直挂着（网关有超时，页面刷新也会断），
// 所以做成"发起 → 轮询"两段式。
type migrateState struct {
	mu        sync.Mutex
	running   bool
	phase     string
	from      string
	to        string
	started   time.Time
	finished  time.Time
	err       string
	result    string
	dismissed bool
}

// migrateResultTTL：迁移结束后，那张卡片还留多久。
//
// 【必须有个头】。迁移成功之后"参数和实际位置不一致"这个条件就不成立了，
// 卡片本该消失；但结果还得让用户看见，所以管理页在 finished 时也显示它。
// 少了这个过期时间，那张卡片就【永远挂在那儿】—— 真机上就是这么表现的：
// 数据早搬完了，页面上还一直摆着「迁移数据目录」。
// 5 分钟足够看清结果，用户也可以点「知道了」立刻收起来。
const migrateResultTTL = 5 * time.Minute

type migrateSnapshot struct {
	Running  bool   `json:"running"`
	Phase    string `json:"phase"`
	From     string `json:"from"`
	To       string `json:"to"`
	Error    string `json:"error"`
	Result   string `json:"result"`
	Elapsed  int64  `json:"elapsed_sec"`
	Finished bool   `json:"finished"`
}

func (m *migrateState) Snapshot() migrateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 结果已经被"知道了"收起来、或者放太久了 → 当作没有这回事，
	// 管理页那张卡片才会消失。正在跑的时候永远照实报。
	if !m.running && !m.finished.IsZero() &&
		(m.dismissed || time.Since(m.finished) > migrateResultTTL) {
		return migrateSnapshot{}
	}
	s := migrateSnapshot{
		Running: m.running, Phase: m.phase, From: m.from, To: m.to,
		Error: m.err, Result: m.result,
		Finished: !m.finished.IsZero(),
	}
	if !m.started.IsZero() {
		end := m.finished
		if m.running || end.IsZero() {
			end = time.Now()
		}
		s.Elapsed = int64(end.Sub(m.started).Seconds())
	}
	return s
}

func (m *migrateState) setPhase(p string) {
	m.mu.Lock()
	m.phase = p
	m.mu.Unlock()
}

// begin 抢占"当前正在迁移"这个位置。抢不到说明已经有一个在跑了。
//
// ⚠ 这里【必须逐个字段赋值】，不能写成 *m = migrateState{...}：
// 那样会把当前正持有的 m.mu 一起覆盖成一把新的零值锁，紧接着的 Unlock
// 解的就是另一把从没锁过的锁，运行时直接 panic。
func (m *migrateState) begin(from, to string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return false
	}
	m.running = true
	m.phase = "正在校验目标目录…"
	m.from, m.to = from, to
	m.started = time.Now()
	m.finished = time.Time{}
	m.err, m.result = "", ""
	m.dismissed = false // 新的一次迁移，上次"知道了"不算数
	return true
}

// Dismiss 把已经结束的那次迁移的结果收起来（用户点了「知道了」）。
// 正在跑的时候不理会 —— 那会让进度显示凭空消失。
func (m *migrateState) Dismiss() {
	m.mu.Lock()
	if !m.running {
		m.dismissed = true
	}
	m.mu.Unlock()
}

func (m *migrateState) finish(err error, result string) {
	m.mu.Lock()
	m.running = false
	m.finished = time.Now()
	m.result = result
	if err != nil {
		m.err = err.Error()
		m.phase = "迁移失败"
	} else {
		m.phase = "迁移完成"
	}
	m.mu.Unlock()
}

// ---- 目标校验 ------------------------------------------------------------

// hasRedisData 判断一个目录里是不是已经有 Redis 的数据了。
func hasRedisData(dir string) bool {
	for _, name := range []string{"dump.rdb", "appendonlydir"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// checkMigrationTarget 在【动手之前】把能拒的都拒掉。
//
// 抽成纯函数是为了能直接对着临时目录测：真机上造"目标里已经有数据"
// "目标嵌在源里面"这些现场很麻烦，而复制到一半才失败的代价是用户白等一次停服、
// 目标目录里还留着半份垃圾。
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
		return fmt.Errorf("数据已经在 %s 了，不用迁移", oldClean)
	}
	// 嵌套会出大事：目标在源里面 → 边复制边把复制出来的东西再复制一遍；
	// 源在目标里面 → 之后给旧目录改名会把新数据一起端走。
	if isSubPath(oldClean, newClean) {
		return fmt.Errorf("目标目录 %s 在当前数据目录里面，不能这样迁移", newClean)
	}
	if isSubPath(newClean, oldClean) {
		return fmt.Errorf("当前数据目录 %s 在目标目录里面，不能这样迁移", oldClean)
	}

	fi, err := os.Stat(newClean)
	if os.IsNotExist(err) {
		return nil // 会被创建出来
	}
	if err != nil {
		return fmt.Errorf("读不了目标目录 %s: %w", newClean, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("目标 %s 已存在且不是目录", newClean)
	}
	entries, err := os.ReadDir(newClean)
	if err != nil {
		return fmt.Errorf("读取目标目录失败: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if hasRedisData(newClean) {
		return fmt.Errorf("目标目录 %s 里已经有 Redis 的数据文件了，不会覆盖它。"+
			"请换一个空目录", newClean)
	}
	return fmt.Errorf("目标目录 %s 不是空的（%d 个条目）。为安全起见只往空目录迁移 ——"+
		"如果里面是上一次迁移失败留下的残骸，可以勾选「清理目标目录后重试」",
		newClean, len(entries))
}

// isSubPath 判断 child 是不是在 parent 里面（相等不算，由调用方另判）。
//
// 【按路径分段比，不能用字符串前缀】—— strings.HasPrefix("/volume1/ab", "/volume1/a")
// 是 true，但 /volume1/ab 根本不在 /volume1/a 里面。
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// clearMigrationTarget 把目标目录里已有的东西【改名让开】，不是删除。
//
// 只在用户明确勾选之后才调，而且拒绝对一个有数据的目录下手 —— 那种情况上面的
// 校验已经拦了，这里再挡一次，因为这是唯一一处会动"别人的目录"的代码。
func clearMigrationTarget(newDir string) (string, error) {
	newDir = filepath.Clean(newDir)
	if hasRedisData(newDir) {
		return "", fmt.Errorf("目标目录里有 Redis 数据文件，拒绝清理")
	}
	retired := newDir + ".abandoned-" + time.Now().Format("20060102-150405")
	if err := os.Rename(newDir, retired); err != nil {
		return "", fmt.Errorf("把 %s 改名到 %s 失败: %w", newDir, retired, err)
	}
	log.Printf("目标目录 %s 里原有的内容已改名为 %s（没有删除）", newDir, retired)
	return retired, nil
}

// ---- 复制 ----------------------------------------------------------------

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
// Bsize 在 linux 上是 int64、在 darwin 上是 uint32，统一转 uint64 两边都能编。
func freeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// copyTree 递归复制，保留权限位。
//
// 遇到符号链接或设备文件【直接报错】而不是跳过：数据目录里本来就不该有这些，
// 静默跳过会复制出一个"看着成功、实际残缺"的副本，那比明确失败糟糕得多。
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
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
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
	// 必须显式 Sync 再 Close：迁移完马上就要在新位置把服务拉起来，
	// 数据还在页缓存里没落盘的话，这期间掉电等于数据没搬过去。
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- 迁移主流程 ----------------------------------------------------------

// StartMigration 校验参数后在后台跑迁移，立刻返回。
// 进度用 a.mig.Snapshot() 轮询。
func (a *App) StartMigration(newDir string, clearTarget bool) error {
	newDir = filepath.Clean(strings.TrimSpace(newDir))
	oldDir := a.DataBase()

	if err := checkMigrationTarget(oldDir, newDir); err != nil {
		if !clearTarget {
			return err
		}
		// 勾了"清理目标"也不是什么都能过：只有"目标非空"这一种拒绝理由能被清理
		// 解决，嵌套、相对路径、目标里有数据文件那些必须照拒。
		if !strings.Contains(err.Error(), "不是空的") {
			return err
		}
		if _, cerr := clearMigrationTarget(newDir); cerr != nil {
			return cerr
		}
		if err := checkMigrationTarget(oldDir, newDir); err != nil {
			return err
		}
	}
	if !a.mig.begin(oldDir, newDir) {
		return fmt.Errorf("已经有一个迁移在进行中")
	}
	go func() {
		err := a.runMigration(oldDir, newDir)
		result := ""
		if err == nil {
			result = "数据已迁移到 " + a.DataBase() + "，Redis 正在新位置启动"
			log.Printf("迁移成功：%s → %s", oldDir, a.DataBase())
		} else {
			log.Printf("::error:: 迁移失败：%v", err)
		}
		a.mig.finish(err, result)
	}()
	return nil
}

func (a *App) runMigration(oldDir, newDir string) error {
	// 这一次统计【只用来估空间】，不能拿去做校验 —— 此刻 Redis 还在跑。
	estBytes, _, err := dirSize(oldDir)
	if err != nil {
		return fmt.Errorf("统计当前数据目录失败: %w", err)
	}
	if err := os.MkdirAll(newDir, 0o700); err != nil {
		return fmt.Errorf("创建目标目录 %s 失败: %w"+
			"（如果它在共享文件夹里，确认安装参数已经选中该目录、并且应用重启过一次）",
			newDir, err)
	}
	// 留 5% 余量：复制期间 Redis 是停的，但同一个卷上可能还有别的应用在写。
	if avail, err := freeSpace(newDir); err == nil {
		need := uint64(float64(estBytes) * 1.05)
		if avail < need {
			return fmt.Errorf("目标所在磁盘可用空间不足：需要约 %s，只剩 %s",
				humanBytes(int64(need)), humanBytes(int64(avail)))
		}
	}

	log.Printf("准备迁移数据目录：%s → %s（约 %s）", oldDir, newDir, humanBytes(estBytes))
	wasRunning := a.rd.Status().Running

	a.mig.setPhase("正在停止 Redis（它会先存一次盘）…")
	a.rd.Stop()

	// ⚠ 源大小必须在【停服之后】重新量一次，绝不能用停服前那份。
	//
	// Redis 收到 SIGTERM 会【先存一次盘再退出】—— 也就是说 dump.rdb 在停服的
	// 那一刻刚被重写，大小和停服前几乎必然不同。拿停服前的数字做校验基准，
	// 会把一次完全正确的复制判成"对不上"而放弃。
	// （MariaDB 那个应用上真栽过同一个坑，只是那边的原因是干净关闭会删掉 ibtmp1。）
	srcBytes, srcFiles, err := dirSize(oldDir)
	if err != nil {
		a.restartAfterFailedMigration(wasRunning)
		return fmt.Errorf("统计当前数据目录失败: %w", err)
	}
	log.Printf("Redis 已停止，开始复制：%d 个文件 / %s", srcFiles, humanBytes(srcBytes))
	a.mig.setPhase(fmt.Sprintf("Redis 已停止，正在复制 %d 个文件 / %s…",
		srcFiles, humanBytes(srcBytes)))

	if err := copyTree(oldDir, newDir); err != nil {
		a.restartAfterFailedMigration(wasRunning)
		return fmt.Errorf("复制数据失败: %w（原数据没有被动过，Redis 仍在原位置；"+
			"重试前请清理 %s）", err, newDir)
	}

	a.mig.setPhase("复制完成，正在核对文件数与字节数…")
	dstBytes, dstFiles, err := dirSize(newDir)
	if err != nil {
		a.restartAfterFailedMigration(wasRunning)
		return fmt.Errorf("核对复制结果失败: %w", err)
	}
	if dstBytes != srcBytes || dstFiles != srcFiles {
		a.restartAfterFailedMigration(wasRunning)
		return fmt.Errorf("复制结果和原数据对不上（原 %d 文件/%s，新 %d 文件/%s），"+
			"已放弃迁移，Redis 仍在原位置", srcFiles, humanBytes(srcBytes),
			dstFiles, humanBytes(dstBytes))
	}
	log.Printf("复制完毕并核对一致：%d 个文件 / %s", dstFiles, humanBytes(dstBytes))

	// 到这里才算成功。先落配置，再改内存里的路径 —— 反过来的话，
	// 配置写失败时内存里已经指着新目录，重启后又回到旧目录，两边不一致。
	if err := a.updateConfig(func(c *Config) { c.DataBase = newDir }); err != nil {
		a.restartAfterFailedMigration(wasRunning)
		return fmt.Errorf("保存新的数据目录失败: %w（数据已经复制到 %s，"+
			"但配置没更新，Redis 仍会在原位置启动）", err, newDir)
	}
	a.setDataBase(newDir)

	// 旧目录改名而不是删除：迁移刚做完就把原始数据删掉太激进了。
	// 改名既能防止"配置万一被重置后又从旧目录启动"，也留了一条后悔路。
	retired := oldDir + ".migrated-" + time.Now().Format("20060102-150405")
	if err := os.Rename(oldDir, retired); err != nil {
		log.Printf("::warning:: 旧数据目录改名失败（%v）。迁移本身已成功，"+
			"Redis 会在新位置启动；旧数据仍在 %s，确认无误后可以手动删除", err, oldDir)
	} else {
		log.Printf("旧数据已改名为 %s —— 确认新位置一切正常后可以手动删除它", retired)
	}

	// 【迁移成功后一定要把服务拉起来，哪怕迁移之前它是停着的】。
	// 用户点这个按钮的意思就是"把数据搬过去然后继续用"，不是"搬完保持原样"。
	a.mig.setPhase("核对通过，正在新位置启动 Redis…")
	if err := a.bootRedis(); err != nil {
		return fmt.Errorf("数据已经搬到 %s 并且核对无误，但在新位置启动失败: %w", newDir, err)
	}
	return nil
}

// restartAfterFailedMigration 把服务恢复到迁移之前的状态。
// 迁移前就是停着的话就保持停着 —— 那多半是用户自己停的，别自作主张。
func (a *App) restartAfterFailedMigration(wasRunning bool) {
	if !wasRunning {
		return
	}
	if err := a.bootRedis(); err != nil {
		log.Printf("::error:: 迁移失败后想把 Redis 恢复到原位置，但启动也失败了: %v", err)
	}
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
