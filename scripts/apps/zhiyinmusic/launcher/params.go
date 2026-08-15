package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// 安装参数。平台把用户在安装/设置弹窗里填的值写进
// <安装目录>/init.d/<appid>.env，unit 用 EnvironmentFile 加载 —— 到这里就是普通环境变量。
//
// ⚠ 全新安装和升级后的【第一次启动，这些值都是空的】：平台先起服务、
//
//	2~3 秒后才写 .env 和 unit。所以每一项都必须能接受"空"，而且空值不是错误、
//	不要打成 error 日志吓人。用户在应用中心停一次再启动就有了。
//
// 只有音乐目录这一个参数。
//
// ⚠ 刻意【不做】"安装时设置管理员账号密码"这种参数：上游首次打开就有引导页
// 让用户创建管理员，再加一份安装参数是画蛇添足 —— 而且首启参数是空的，
// 那份参数注定要到第二次启动才生效，反而制造出"我明明填了却没用"的困惑。
const envMusicPaths = "ZHIYIN_MUSIC_PATHS"

type Params struct {
	MusicPaths []string
}

func readParams(getenv func(string) string) Params {
	return Params{MusicPaths: parsePathList(getenv(envMusicPaths))}
}

// parsePathList 解析 multi: true 的 path 型参数。
//
// 平台给的是【JSON 数组】（`["/volume1/a","/volume1/b"]`），一个都没选时是
// 字面量 `null`（空列表 marshal 的结果），不是空字符串 —— 判空要把它算上。
//
// 刻意不按分隔符切：路径里完全可能有逗号和空格。
func parsePathList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}

	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		// 不是合法 JSON 数组时按单个裸路径处理：multi 从 true 改回 false 的话
		// 平台就会写裸标量，这种时候没必要整份丢掉。
		if strings.HasPrefix(raw, "/") {
			list = []string{raw}
		} else {
			return nil
		}
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, p := range list {
		p = strings.TrimSpace(p)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out) // 顺序稳定，日志和状态文件才好比对
	return out
}
