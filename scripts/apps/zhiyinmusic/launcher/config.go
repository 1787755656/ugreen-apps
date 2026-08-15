package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"strconv"
	"strings"
)

// 上游的配置全在一个 config.toml 里，而且是【相对 cwd】解析的
// （在容器里做过对照实验：cwd=/elsewhere 就读 /elsewhere/config.toml，
// cwd=/ 读不到就整份用默认值 —— 默认值里 database.path 是 /data/db.sqlite，
// 在绿联沙箱里写不进去，所以这个文件是必须要有的）。
//
// 除了下面这几项，其余配置一律是用户的事：应用里有完整的设置界面
// （PUT /api/config 会把整份 config.toml 写回来），所以这里【只改这几个键，
// 其余原样保留】—— 用户改过的采样并发、缓存大小、刮削源不能被我们冲掉。
//
//	[server]    host / port      端口由平台声明，必须一致
//	[database]  path             安装目录只读，数据库得落在数据目录
//	[covers]    cache_path       同上（封面库丢了要重扫全库，所以放 Data 不放 Cache）
//	[transcode] cache_path       转码产物可重建，放 Cache
//	[web]       enabled / path   前端在安装目录里，升级后路径可能变，每次都重写
//	[scan]      roots            安装参数里选的音乐目录，和用户在应用里加的合并
type managedKey struct {
	section string
	key     string
	value   string // 已经是 TOML 字面量（字符串要自带引号）
}

// syncConfig 生成或就地更新 config.toml，返回这次对扫描目录的增删。
func syncConfig(p Paths, port int, params Params) (added, removed []string, err error) {
	src, fresh, err := loadOrTemplate(p)
	if err != nil {
		return nil, nil, err
	}

	existing := parseStringArray(src, "scan", "roots")
	if fresh {
		// 模板里的 roots 是 Docker 的 ["/music"]，在绿联上不存在，别继承。
		existing = nil
	}
	roots, added, removed := mergeRoots(p, existing, params.MusicPaths)

	edits := []managedKey{
		// JWT 密钥。上游不配的话【每次启动都重新随机生成】，于是所有人的登录态
		// 在应用重启后全部失效（日志里就一句 WARN）。而绿联这边重启是家常便饭 ——
		// 光是"首启参数为空、停一次再启动"这套流程就至少一次。
		//
		// ⚠ 只在配置里还没有这一项时才生成：已经有的绝不能覆盖，否则每次启动
		//    都是一次"全体重新登录"。上游的模板里它是注释掉的，所以首次会命中。
		{"security", "jwt_secret", quote(ensureSecret(src))},
		// 0.0.0.0 而不是 127.0.0.1：tab 型应用是浏览器直连这个端口的，
		// 手机上的 Subsonic 客户端（音流、Symfonium）也要连它。
		{"server", "host", `"0.0.0.0"`},
		{"server", "port", strconv.Itoa(port)},
		{"database", "path", quote(p.Database())},
		{"covers", "cache_path", quote(p.CoverStore())},
		{"transcode", "cache_path", quote(p.TranscodeCache())},
		{"web", "enabled", "true"},
		{"web", "path", quote(p.Web)},
		{"scan", "roots", tomlStringArray(roots)},
	}

	out := applyManaged(src, edits)
	if err := writeFileAtomic(p.ConfigFile(), out); err != nil {
		return nil, nil, err
	}
	if err := saveRootsState(p, params.MusicPaths); err != nil {
		// 不致命：下次启动最多是少一次"移出"的收敛。
		return added, removed, nil
	}
	return added, removed, nil
}

// ensureSecret 返回配置里已有的 jwt_secret；没有就生成一个新的。
//
// 生成失败（熵源出问题这种基本不会发生的事）时返回空串 —— 写成
// jwt_secret = "" 上游会当没配，退回随机密钥，也就是回到不做这件事的状态，
// 不至于让应用起不来。
func ensureSecret(src string) string {
	if v := parseScalarString(src, "security", "jwt_secret"); v != "" {
		return v
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// configuredRoots 读回【最终写进配置文件的】扫描目录。
//
// 刻意不直接用安装参数：用户在应用里自己加的目录也要一起诊断，
// 而那些只存在于 config.toml 里。
func configuredRoots(p Paths) []string {
	b, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		return nil
	}
	return parseStringArray(string(b), "scan", "roots")
}

func loadOrTemplate(p Paths) (src string, fresh bool, err error) {
	if b, err := os.ReadFile(p.ConfigFile()); err == nil {
		return string(b), false, nil
	}
	b, err := os.ReadFile(p.ConfigTemplate())
	if err != nil {
		return "", false, fmt.Errorf("读配置模板 %s 失败：%w", p.ConfigTemplate(), err)
	}
	return string(b), true, nil
}

// mergeRoots 把安装参数里的音乐目录并进用户已有的扫描目录。
//
// 规则：
//   - 参数里新增的目录 → 加进去；
//   - 用户在应用里自己加的目录 → 永远不动；
//   - 【上一次是我们注入的、这次参数里没有了】→ 移出去。
//     只有靠这份状态才分得清"用户自己加的"和"我们注入的"，
//     不然要么参数改了不生效，要么把用户加的目录冲掉。
func mergeRoots(p Paths, existing, params []string) (roots, added, removed []string) {
	prev := loadRootsState(p)
	inParams := toSet(params)
	inPrev := toSet(prev)

	seen := map[string]bool{}
	for _, d := range existing {
		if inPrev[d] && !inParams[d] {
			removed = append(removed, d)
			continue
		}
		if !seen[d] {
			seen[d] = true
			roots = append(roots, d)
		}
	}
	for _, d := range params {
		if !seen[d] {
			seen[d] = true
			roots = append(roots, d)
			added = append(added, d)
		}
	}
	return roots, added, removed
}

func loadRootsState(p Paths) []string {
	b, err := os.ReadFile(p.RootsState())
	if err != nil {
		return nil
	}
	var list []string
	if json.Unmarshal(b, &list) != nil {
		return nil
	}
	return list
}

func saveRootsState(p Paths, params []string) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return writeFileAtomic(p.RootsState(), string(b))
}

// ---------------------------------------------------------------------------
// 一个够用的 TOML 就地编辑器
//
// 刻意不引第三方 TOML 库、也刻意不做"解析成 map 再整份重新序列化"：
// 后者会把上游那份写得很详细的中文注释全部丢掉，而用户在绿联上是没有 SSH 的，
// 那些注释就是他唯一能看到的配置说明。这里只替换指定的 (section, key) 行，
// 其余字节原样保留。

var sectionRe = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(?:#.*)?$`)

func keyLineRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`^(\s*)` + regexp.QuoteMeta(key) + `\s*=`)
}

// applyManaged 就地替换若干 (section, key) 的值；section 或 key 不存在就补上。
func applyManaged(src string, edits []managedKey) string {
	lines := strings.Split(src, "\n")
	done := make(map[string]bool, len(edits))

	var out []string
	section := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			section = strings.TrimSpace(m[1])
			out = append(out, line)
			continue
		}

		replaced := false
		for _, e := range edits {
			if e.section != section || done[e.section+"."+e.key] {
				continue
			}
			if !keyLineRe(e.key).MatchString(line) {
				continue
			}
			// 值可能跨行（数组）——把原值整段吃掉再写新的一行。
			j := i
			for !valueComplete(strings.Join(lines[i:j+1], "\n")) && j+1 < len(lines) {
				j++
			}
			i = j
			out = append(out, e.key+" = "+e.value)
			done[e.section+"."+e.key] = true
			replaced = true
			break
		}
		if !replaced {
			out = append(out, line)
		}
	}

	// 补上没找到的：有 section 就插在 section 头后面，没有就在文件末尾新建。
	for _, e := range edits {
		if done[e.section+"."+e.key] {
			continue
		}
		out = insertIntoSection(out, e)
		done[e.section+"."+e.key] = true
	}
	return strings.Join(out, "\n")
}

func insertIntoSection(lines []string, e managedKey) []string {
	for i, line := range lines {
		m := sectionRe.FindStringSubmatch(line)
		if m == nil || strings.TrimSpace(m[1]) != e.section {
			continue
		}
		row := e.key + " = " + e.value
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:i+1]...)
		out = append(out, row)
		out = append(out, lines[i+1:]...)
		return out
	}
	return append(lines, "", "["+e.section+"]", e.key+" = "+e.value)
}

// valueComplete 判断从 `key =` 开始的这段文本是不是已经是一个完整的值。
// 只需要认数组的方括号配平 —— 这份配置里跨行的只有数组。
func valueComplete(chunk string) bool {
	depth := 0
	inStr := byte(0)
	esc := false
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch {
		case esc:
			esc = false
		case inStr != 0:
			if c == '\\' && inStr == '"' {
				esc = true
			} else if c == inStr {
				inStr = 0
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '#':
			// 注释到行尾
			for i < len(chunk) && chunk[i] != '\n' {
				i++
			}
		case c == '[':
			depth++
		case c == ']':
			depth--
		}
	}
	return depth <= 0
}

// parseScalarString 取出 (section, key) 处的单个字符串值；没有或不是字符串就返回空串。
func parseScalarString(src, section, key string) string {
	lines := strings.Split(src, "\n")
	cur := ""
	for _, line := range lines {
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			cur = strings.TrimSpace(m[1])
			continue
		}
		if cur != section || !keyLineRe(key).MatchString(line) {
			continue
		}
		if vals := extractStrings(line); len(vals) > 0 {
			return vals[0]
		}
		return ""
	}
	return ""
}

// parseStringArray 取出 (section, key) 处的字符串数组，认跨行写法。
func parseStringArray(src, section, key string) []string {
	lines := strings.Split(src, "\n")
	cur := ""
	for i := 0; i < len(lines); i++ {
		if m := sectionRe.FindStringSubmatch(lines[i]); m != nil {
			cur = strings.TrimSpace(m[1])
			continue
		}
		if cur != section || !keyLineRe(key).MatchString(lines[i]) {
			continue
		}
		j := i
		for !valueComplete(strings.Join(lines[i:j+1], "\n")) && j+1 < len(lines) {
			j++
		}
		return extractStrings(strings.Join(lines[i:j+1], "\n"))
	}
	return nil
}

// extractStrings 抠出一段 TOML 文本里的所有字符串字面量（基本串和字面串）。
func extractStrings(chunk string) []string {
	var out []string
	var buf strings.Builder
	inStr := byte(0)
	esc := false
	for i := 0; i < len(chunk); i++ {
		c := chunk[i]
		switch {
		case esc:
			// TOML 的转义只有这几个在路径里可能出现，其余原样带过。
			switch c {
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			default:
				buf.WriteByte(c)
			}
			esc = false
		case inStr != 0:
			switch {
			case c == '\\' && inStr == '"':
				esc = true
			case c == inStr:
				out = append(out, buf.String())
				buf.Reset()
				inStr = 0
			default:
				buf.WriteByte(c)
			}
		case c == '"' || c == '\'':
			inStr = c
		case c == '#':
			for i < len(chunk) && chunk[i] != '\n' {
				i++
			}
		}
	}
	return out
}

func tomlStringArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, quote(s))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func toSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, s := range list {
		m[s] = true
	}
	return m
}

// writeFileAtomic 先写临时文件再改名 —— 断电/写坏一半的话，
// 用户看到的是"配置没生效"，比"配置文件语法错误、应用起不来"好得多。
func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
