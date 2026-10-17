package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func openRead(path string) (*os.File, error) { return os.Open(path) }

// resolveTarget 解析要下载/删除的文件：
//   - path 参数：绝对路径，必须落在授权根目录内
//   - file 参数：下载根目录下的扁平文件名（兼容旧参数，拒绝路径穿越）
func (s *server) resolveTarget(r *http.Request) (string, error) {
	if p := strings.TrimSpace(r.FormValue("path")); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("路径必须是绝对路径")
		}
		p = filepath.Clean(p)
		if !underAny(p, s.paths.allowedRoots(s.store.taskOutDirs()...)) {
			return "", fmt.Errorf("路径不在允许范围内")
		}
		return p, nil
	}
	name := strings.TrimSpace(r.FormValue("file"))
	if name == "" {
		return "", fmt.Errorf("缺少文件参数")
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") || name != filepath.Base(name) {
		return "", fmt.Errorf("非法文件名")
	}
	return filepath.Join(s.paths.Download, name), nil
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveTarget(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	st, err := os.Stat(target)
	if err != nil || st.IsDir() {
		writeJSON(w, 404, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	f, err := os.Open(target)
	if err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": "打开文件失败: " + err.Error()})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(target)))
	http.ServeContent(w, r, filepath.Base(target), st.ModTime(), f)
}

func (s *server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	target, err := s.resolveTarget(r)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	st, err := os.Stat(target)
	if err != nil || st.IsDir() {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "文件不存在"})
		return
	}
	if err := os.Remove(target); err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "删除失败: " + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleDirs 是内置的目录浏览器后端（替代 fnOS 的文件选择器 SDK）。
// 只做展示；能否真正写入仍由 Create/delete 的授权根校验决定。
func (s *server) handleDirs(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.FormValue("path"))
	roots := s.paths.allowedRoots(s.store.taskOutDirs()...)
	if p == "" || p == "null" {
		items := make([]dirEntry, 0, len(roots))
		for _, root := range roots {
			items = append(items, dirEntry{Name: labelForRoot(root), Path: root})
		}
		if len(items) == 0 {
			items = append(items, dirEntry{Name: "/", Path: "/"})
		}
		writeJSON(w, 200, map[string]any{
			"ok": true, "path": "", "parent": "", "dirs": items, "selectable": false,
		})
		return
	}
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "路径必须是绝对路径"})
		return
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "无法读取目录: " + err.Error()})
		return
	}
	dirs := []dirEntry{}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dirs = append(dirs, dirEntry{Name: e.Name(), Path: filepath.Join(p, e.Name())})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "path": p, "parent": parent, "dirs": dirs,
		"selectable": underAny(p, roots),
		"writable":   isWritableDir(p),
	})
}

func labelForRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func isWritableDir(p string) bool {
	f, err := os.CreateTemp(p, ".wtest")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}
