package main

import (
	"bytes"
	"strings"
)

// qBittorrent.conf 是 Qt QSettings 写的 INI。这里做最小化的"按行编辑"，
// 不认识的行原样保留 —— 配置里那条 15KB 的 tracker 列表和 @ByteArray 密码
// 都不是标准 INI 能安全往返的东西，整体重写风险太大。
type iniFile struct {
	lines []string
}

func parseINI(data []byte) *iniFile {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return &iniFile{}
	}
	return &iniFile{lines: strings.Split(text, "\n")}
}

func sectionOf(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1], true
	}
	return "", false
}

// splitKey 拆出一行的 key，注释行和无 '=' 的行返回 false。
func splitKey(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if s == "" || s[0] == '#' || s[0] == ';' || s[0] == '[' {
		return "", false
	}
	i := strings.Index(s, "=")
	if i < 0 {
		return "", false
	}
	return strings.TrimSpace(s[:i]), true
}

func (f *iniFile) Get(section, key string) (string, bool) {
	cur := ""
	for _, line := range f.lines {
		if s, ok := sectionOf(line); ok {
			cur = s
			continue
		}
		if cur != section {
			continue
		}
		if k, ok := splitKey(line); ok && k == key {
			_, v, _ := strings.Cut(line, "=")
			return v, true
		}
	}
	return "", false
}

func (f *iniFile) HasSection(section string) bool {
	for _, line := range f.lines {
		if s, ok := sectionOf(line); ok && s == section {
			return true
		}
	}
	return false
}

// Set 就地替换已有的键；键不存在就追加到该节末尾；节不存在就新建一节。
func (f *iniFile) Set(section, key, value string) {
	cur := ""
	sectionEnd := -1 // 该节最后一个非空行的下标
	for i, line := range f.lines {
		if s, ok := sectionOf(line); ok {
			if cur == section {
				break // 已经走出目标节
			}
			cur = s
			if cur == section {
				sectionEnd = i
			}
			continue
		}
		if cur != section {
			continue
		}
		if k, ok := splitKey(line); ok && k == key {
			f.lines[i] = key + "=" + value
			return
		}
		if strings.TrimSpace(line) != "" {
			sectionEnd = i
		}
	}
	if sectionEnd < 0 {
		if len(f.lines) > 0 {
			f.lines = append(f.lines, "")
		}
		f.lines = append(f.lines, "["+section+"]", key+"="+value)
		return
	}
	f.lines = append(f.lines, "")
	copy(f.lines[sectionEnd+2:], f.lines[sectionEnd+1:])
	f.lines[sectionEnd+1] = key + "=" + value
}

func (f *iniFile) Bytes() []byte {
	var buf bytes.Buffer
	for _, line := range f.lines {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// qBittorrent 5.x 的键位迁移
//
// 5.x 首次启动会把老的 [Preferences] 键搬进 [BitTorrent]（并写下
// [Meta] MigrationVersion），之后**只读新键**。superng6 镜像里带的那份默认配置
// 是老键写法，靠的就是这次迁移。
//
// 所以改配置必须分情况：迁移过的文件只改老键 = 完全不生效（而且不报错）。
// 规则：老键始终写；已经迁移过的，新键也一起写，让两边一致。
// ---------------------------------------------------------------------------

func (f *iniFile) migrated() bool {
	if _, ok := f.Get("Meta", "MigrationVersion"); ok {
		return true
	}
	return f.HasSection("BitTorrent")
}

type confKey struct {
	legacySection, legacyKey string
	modernSection, modernKey string
}

var (
	keySavePath = confKey{"Preferences", `Downloads\SavePath`, "BitTorrent", `Session\DefaultSavePath`}
	keyTempPath = confKey{"Preferences", `Downloads\TempPath`, "BitTorrent", `Session\TempPath`}
	keyBTPort   = confKey{"Preferences", `Connection\PortRangeMin`, "BitTorrent", `Session\Port`}
	keyTrackers = confKey{"Preferences", `Bittorrent\TrackersList`, "BitTorrent", `Session\AdditionalTrackers`}
)

func (f *iniFile) SetPref(k confKey, value string) {
	f.Set(k.legacySection, k.legacyKey, value)
	if f.migrated() {
		f.Set(k.modernSection, k.modernKey, value)
	}
}

func (f *iniFile) GetPref(k confKey) (string, bool) {
	if f.migrated() {
		if v, ok := f.Get(k.modernSection, k.modernKey); ok {
			return v, true
		}
	}
	return f.Get(k.legacySection, k.legacyKey)
}
