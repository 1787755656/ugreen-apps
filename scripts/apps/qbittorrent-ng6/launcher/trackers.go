package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 复刻镜像里 cont-init.d/40-config 的行为：启动前拉一份 tracker 列表，
// 替换掉配置里的 Bittorrent\TrackersList。
//
// 只在启动时更新一次（和上游一致）。不做定时更新是有意的：qBittorrent 运行中
// 会整体重写 qBittorrent.conf，跑着的时候改文件会被它覆盖掉。

const trackerFetchLimit = 4 << 20 // 4MiB，别让一个跑偏的 URL 把内存吃光

func fetchTrackerList(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, trackerFetchLimit))
	if err != nil {
		return "", err
	}
	list := normalizeTrackerList(string(body))
	if list == "" {
		return "", fmt.Errorf("列表内容为空")
	}
	return list, nil
}

// normalizeTrackerList 去掉空行，再把换行编码成 Qt INI 里的字面量 \n
//（上游那句 awk 去空行 + sed 把换行换成 \n 的等价物）。
func normalizeTrackerList(body string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Qt 的 INI 拿逗号当 QStringList 分隔符，混进来会把整条值切碎。
		if strings.ContainsAny(line, ",\\") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, `\n`)
}
