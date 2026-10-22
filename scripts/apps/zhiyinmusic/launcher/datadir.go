package main

// 「配置文件夹」—— 也就是应用自己的数据目录（config.toml / db.sqlite / 封面库 /
// Subsonic 的 pepper 全在里面）。
//
// 平台给每个原生应用固定一个数据目录（unit 里的 UGAPP_DATA_DIR，实际落在
// /volume{N}/@appdata/<appid>）。它有两个问题：
//
//	① 用户在文件管理器里看不见，想备份只能整机备份或者上 SSH；
//	② 想换块盘（比如小容量系统盘换到大容量数据盘）根本无从下手。
//
// 所以再声明一个 type: path 的安装参数 ZHIYIN_DATA_DIR：用户在应用中心的
// 安装/设置弹窗里选一个文件夹，既拿到路径、又顺带把这个目录授权进沙箱
// （平台会写 path_permissions → unit 的 BindPaths=，见 project.yaml 的注释）。
//
// 三条铁律，全是踩过才知道的：
//
//  1. 平台数据目录永远是"锚"。状态文件 .ugos-data-dir 只写在那儿 ——
//     用户把参数清空时，只能靠它知道当前真正的数据在哪个自定义目录里。
//  2. 搬家 = 复制 + 校验 + 把旧数据挪进 .migrated-<时间戳>/，一个字节都不删。
//     用户手滑选错目录不该丢曲库。
//  3. 搬完必须让旧位置"看起来没有数据"（这就是第 2 条要挪走而不是留下的原因）。
//     否则用户之后把参数改回来，程序会把那份过期数据当成"新目录里已经有数据"
//     直接采用，等于静默回滚 —— 比报错难查得多。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const dataDirStateName = ".ugos-data-dir"

// isMarker 判断是不是我们自己的记号（不参与搬迁、也不参与"有没有数据"的判断）。
func isMarker(name string) bool {
	return name == dataDirStateName ||
		name == dataDirStateName+".tmp" ||
		strings.HasPrefix(name, ".migrated-")
}

// activeDataNames —— 有其一就认为"这个目录里已经有知音音乐的数据"。
// config.toml 一启动就会写，db.sqlite 是上游扫描后建的，两个都够用了。
var activeDataNames = []string{"config.toml", "db.sqlite"}

func hasActiveData(dir string) bool {
	for _, n := range activeDataNames {
		if st, err := os.Stat(filepath.Join(dir, n)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

var volumeRootRe = regexp.MustCompile(`^/volume\d+$`)

// useDataDir 决定这次用哪个数据目录，必要时把旧数据搬过去，最后写回状态文件。
//
// 返回值是给日志用的说明（顺序即阅读顺序）。它【永远不返回 error】：
// 配置文件夹选错了最多是"这次没搬成、继续用原来的目录"，
// 绝不能因为一个安装参数让应用起不来 —— 用户还得进应用去改呢。
func (p *Paths) useDataDir(params Params) []string {
	var notes []string

	prev := readDataDirState(p.PlatformData)
	if prev == "" {
		prev = p.PlatformData
	}

	desired := prev
	if raw := strings.TrimSpace(params.DataDir); raw != "" {
		clean, err := p.validateDataDir(raw)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf(
				"⚠ 安装参数里的「配置文件夹」%s 不能用（%v），这次继续用 %s。", raw, err, prev))
		default:
			if err := ensureWritable(clean); err != nil {
				// 首启授权还没生效时必然走到这里（平台先起服务、几秒后才写 unit）。
				notes = append(notes, fmt.Sprintf(
					"⚠ 配置文件夹 %s 写不进去（%v）—— 多半是安装时的目录授权还没生效。"+
						"这次继续用 %s；去应用中心把本应用「停止 → 启动」一次，它就会搬过去。",
					clean, err, prev))
			} else {
				desired = clean
			}
		}
	}

	if !samePath(desired, prev) {
		ok, migrateNotes := migrateData(prev, desired)
		notes = append(notes, migrateNotes...)
		if !ok {
			desired = prev // 没搬成，老老实实留在原地
		}
	}

	if samePath(desired, p.PlatformData) {
		notes = append(notes, "配置文件夹：没有指定，用应用自己的数据目录 "+desired)
	} else {
		notes = append(notes, "配置文件夹："+desired+
			"（备份这一个文件夹就能保住曲库索引、封面和 Subsonic 登录态）")
	}

	// 收尾再确认一次可写。走到这里还写不进去，最典型的情况是"配置文件夹在外接盘上，
	// 盘没插"。这时【绝不自动改回默认目录】：那会让应用带着空曲库起来，
	// 用户以为数据丢了。宁可起不来，也要把话说明白（错误由 ensureDirs 抛出）。
	if err := ensureWritable(desired); err != nil {
		notes = append(notes, fmt.Sprintf(
			"⚠ 配置文件夹 %s 现在写不进去（%v）。如果它在外接盘或网络位置上，"+
				"先把它挂回来再启动 —— 不会自动改回默认目录，免得你以为曲库丢了。",
			desired, err))
	}

	if err := writeDataDirState(p.PlatformData, desired); err != nil {
		notes = append(notes, "⚠ 记录配置文件夹位置失败（下次启动会再判断一次，不影响使用）："+err.Error())
	}

	p.Data = desired
	return append(notes, p.nestingNotes(params)...)
}

// validateDataDir 挡住那些一眼就不该选的路径。
func (p *Paths) validateDataDir(raw string) (string, error) {
	c := filepath.Clean(raw)
	if !filepath.IsAbs(c) {
		return "", fmt.Errorf("不是绝对路径")
	}
	if filepath.Dir(c) == c {
		return "", fmt.Errorf("不能是整个根目录")
	}
	if volumeRootRe.MatchString(c) {
		return "", fmt.Errorf("不能是整个存储空间，请选它下面的一个文件夹")
	}
	if samePath(c, p.Root) || isInside(c, p.Root) {
		return "", fmt.Errorf("落在安装目录里，那儿是只读的")
	}
	if samePath(c, p.Cache) || isInside(p.Cache, c) {
		return "", fmt.Errorf("和缓存目录冲突，缓存是随时可以删的")
	}
	return c, nil
}

// ensureWritable 建目录并真写一个探针文件。
//
// 只 MkdirAll 是不够的：未授权的目录在沙箱里表现成"父目录只读"（EROFS/EACCES），
// 而已经存在的目录也可能因为授权的是别的层级而写不进去。
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".write-probe")
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// 写一个字节再关，别只 open —— 有些只读挂载是在 write 才报错的。
	if _, err := f.Write([]byte("ok")); err != nil {
		f.Close()
		os.Remove(probe)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(probe)
		return err
	}
	return os.Remove(probe)
}

// migrateData 把 prev 里的数据搬到 desired。
//
// ok=false 表示"没搬成，别切过去"。两种情况都算成功：
//   - 旧位置本来就没数据（首次安装就选了文件夹，直接建空目录就行）；
//   - 新位置里已经有知音音乐的数据（用户把参数指回一份旧备份），那就以它为准，
//     绝不覆盖 —— 覆盖用户数据是最不可挽回的一种 bug。
func migrateData(prev, desired string) (ok bool, notes []string) {
	if !isDir(prev) || !hasEntries(prev) {
		if err := os.MkdirAll(desired, 0o755); err != nil {
			return false, []string{"⚠ 建配置文件夹 " + desired + " 失败：" + err.Error()}
		}
		return true, nil
	}

	if hasActiveData(desired) {
		notes = append(notes, fmt.Sprintf(
			"配置文件夹 %s 里已经有知音音乐的数据，这次直接用它；%s 里的旧数据原样保留。",
			desired, prev))
	} else {
		files, bytes, err := copyDataTree(prev, desired)
		if err != nil {
			return false, []string{fmt.Sprintf(
				"⚠ 把数据从 %s 搬到 %s 失败（%v），这次继续用原来的目录 —— "+
					"一个文件都没动，处理好权限后再启动一次就会重试。", prev, desired, err)}
		}
		// 校验：数量对不上就别切（复制过程中断电/磁盘满是真实场景）。
		sf, sb := treeStats(prev)
		df, db := treeStats(desired)
		if sf != df || sb != db {
			return false, []string{fmt.Sprintf(
				"⚠ 搬到 %s 的数据不完整（旧 %d 个文件/%s，新 %d 个/%s），"+
					"这次继续用原来的目录，一个文件都没动。", desired,
				sf, humanBytes(sb), df, humanBytes(db))}
		}
		notes = append(notes, fmt.Sprintf("已把 %d 个文件（%s）搬进 %s", files, humanBytes(bytes), desired))
	}

	// 把旧位置腾空 —— 见文件头第 3 条：留着会让"改回来"变成静默回滚。
	if aside, err := moveAside(prev); err != nil {
		notes = append(notes, "⚠ 旧位置 "+prev+" 里的数据没能挪开（"+err.Error()+
			"）。如果之后把「配置文件夹」改回这里，请先手动清空它。")
	} else if aside != "" {
		notes = append(notes, "旧位置的数据已挪到 "+aside+"，确认新位置一切正常之后可以自己删掉")
	}
	return true, notes
}

// copyDataTree 逐项复制（不跟软链、不搬自己的记号文件）。
func copyDataTree(src, dst string) (files int, bytes int64, err error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return 0, 0, err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if isMarker(e.Name()) {
			continue
		}
		n, b, err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
		files += n
		bytes += b
		if err != nil {
			return files, bytes, fmt.Errorf("%s：%w", e.Name(), err)
		}
	}
	return files, bytes, nil
}

func copyTree(src, dst string) (files int, bytes int64, err error) {
	err = filepath.WalkDir(src, func(pth string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, relErr := filepath.Rel(src, pth)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			mode := os.FileMode(0o755)
			if info, ierr := d.Info(); ierr == nil {
				mode = info.Mode().Perm()
			}
			return os.MkdirAll(target, mode)
		}
		// 设备/管道/软链一律跳过：上游只写普通文件，搬别的没意义还可能挂死。
		if !d.Type().IsRegular() {
			return nil
		}
		n, cerr := copyFile(pth, target)
		if cerr != nil {
			return cerr
		}
		files++
		bytes += n
		return nil
	})
	return files, bytes, err
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		out.Close()
		return n, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return n, err
	}
	return n, out.Close()
}

// treeStats 数一个目录里的普通文件数和总字节数（跳过记号文件），用于搬迁后校验。
func treeStats(dir string) (files int, bytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var visit func(p string)
	visit = func(p string) {
		items, ierr := os.ReadDir(p)
		if ierr != nil {
			return
		}
		for _, it := range items {
			if isMarker(it.Name()) {
				continue
			}
			full := filepath.Join(p, it.Name())
			if it.IsDir() {
				visit(full)
				continue
			}
			info, ierr := it.Info()
			if ierr != nil || !info.Mode().IsRegular() {
				continue
			}
			files++
			bytes += info.Size()
		}
	}
	for _, e := range entries {
		if isMarker(e.Name()) {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			visit(full)
			continue
		}
		if info, ierr := e.Info(); ierr == nil && info.Mode().IsRegular() {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes
}

// moveAside 把 dir 里的数据整体挪进 dir/.migrated-<时间戳>/，返回那个目录的位置。
// 一个文件都不删 —— 用户确认无误后自己删。
func moveAside(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var moving []string
	for _, e := range entries {
		if !isMarker(e.Name()) {
			moving = append(moving, e.Name())
		}
	}
	if len(moving) == 0 {
		return "", nil
	}
	dest := filepath.Join(dir, ".migrated-"+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	for _, name := range moving {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dest, name)); err != nil {
			return dest, err
		}
	}
	return dest, nil
}

// nestingNotes 检查配置文件夹和音乐目录有没有互相嵌套。
//
// ⚠ 平台的授权是按目录树来的：父子目录【同时】被选成参数时，父目录反而不会被
// bind（它退化成一个只含已授权子目录的合成视图）。表现出来就是"音乐突然全没了"，
// 而日志里什么都看不出来 —— 所以在启动时点名说清楚。
func (p *Paths) nestingNotes(params Params) []string {
	var out []string
	for _, m := range params.MusicPaths {
		switch {
		case samePath(m, p.Data):
			out = append(out, "⚠ 配置文件夹和音乐文件夹选成了同一个目录（"+m+"）。"+
				"能用，但音乐文件会和数据库、封面混在一起，建议分开。")
		case isInside(p.Data, m):
			out = append(out, "⚠ 配置文件夹 "+p.Data+" 在音乐文件夹 "+m+" 里面。"+
				"平台在父子目录同时被选中时不会授权父目录 —— 请把它们改成两个互不包含的目录，"+
				"否则很可能扫不到音乐。")
		case isInside(m, p.Data):
			out = append(out, "⚠ 音乐文件夹 "+m+" 在配置文件夹 "+p.Data+" 里面。"+
				"平台在父子目录同时被选中时不会授权父目录 —— 请把它们改成两个互不包含的目录，"+
				"否则很可能扫不到音乐。")
		}
	}
	return out
}

func readDataDirState(anchor string) string {
	b, err := os.ReadFile(filepath.Join(anchor, dataDirStateName))
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(b))
	if !filepath.IsAbs(v) {
		return ""
	}
	return filepath.Clean(v)
}

func writeDataDirState(anchor, dir string) error {
	if err := os.MkdirAll(anchor, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(anchor, dataDirStateName), dir+"\n")
}

// samePath / isInside —— 都按 clean 后的路径比，避免尾部斜杠和 .. 造成误判。
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// isInside 判断 child 是否（严格地）在 parent 里面。
func isInside(child, parent string) bool {
	c, p := filepath.Clean(child), filepath.Clean(parent)
	if c == p {
		return false
	}
	return strings.HasPrefix(c, strings.TrimSuffix(p, "/")+"/")
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !isMarker(e.Name()) {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}
