package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeBundledGeoIP(t *testing.T, installDir string, payload []byte) {
	t.Helper()
	src := filepath.Join(installDir, geoipBundledRel)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func geoipTarget(l layout) string {
	return filepath.Join(l.profileDir, "qBittorrent", "data", geoipDirName, geoipFileName)
}

func TestGeoIPInstalledOnFirstBoot(t *testing.T) {
	l := newTestLayout(t)
	install := t.TempDir()
	payload := bytes.Repeat([]byte("MMDB"), geoipMinSize) // 够大，过得了完整性下限

	writeBundledGeoIP(t, install, payload)
	ensureGeoIPDatabase(l, install)

	got, err := os.ReadFile(geoipTarget(l))
	if err != nil {
		t.Fatalf("自带的地理库没铺过去：%v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("解压出来的内容和源文件不一致")
	}
}

// qBittorrent 自己每月会更新这个库，已经有的绝不能被包内那份旧的盖回去。
func TestGeoIPDoesNotOverwriteExisting(t *testing.T) {
	l := newTestLayout(t)
	install := t.TempDir()
	writeBundledGeoIP(t, install, bytes.Repeat([]byte("OLD!"), geoipMinSize))

	target := geoipTarget(l)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("qBittorrent 自己下的新版本"), 0o644); err != nil {
		t.Fatal(err)
	}

	ensureGeoIPDatabase(l, install)

	got, _ := os.ReadFile(target)
	if string(got) != "qBittorrent 自己下的新版本" {
		t.Fatal("已存在的地理库被包内那份覆盖了")
	}
}

// 地理库出任何问题都只是看不到 peer 的国旗，绝不能影响启动。
func TestGeoIPFailuresAreNotFatal(t *testing.T) {
	l := newTestLayout(t)

	ensureGeoIPDatabase(l, t.TempDir()) // 包里根本没有这个文件
	if _, err := os.Stat(geoipTarget(l)); !os.IsNotExist(err) {
		t.Fatal("没有源文件时不该凭空产出目标文件")
	}

	// 半截 / 损坏的 gz
	install := t.TempDir()
	src := filepath.Join(install, geoipBundledRel)
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("这不是 gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	ensureGeoIPDatabase(l, install)
	if _, err := os.Stat(geoipTarget(l)); !os.IsNotExist(err) {
		t.Fatal("坏掉的源文件不该产出目标文件")
	}
}

// 解压出来太小说明包里的东西不对，宁可不铺，也别让 qBittorrent 去加载半个库。
func TestGeoIPRejectsTruncatedPayload(t *testing.T) {
	l := newTestLayout(t)
	install := t.TempDir()
	writeBundledGeoIP(t, install, []byte("太小了"))

	ensureGeoIPDatabase(l, install)
	if _, err := os.Stat(geoipTarget(l)); !os.IsNotExist(err) {
		t.Fatal("过小的库不该被铺过去")
	}
	// 临时文件也不能留在目标目录里
	entries, err := os.ReadDir(filepath.Dir(geoipTarget(l)))
	if err == nil {
		for _, e := range entries {
			t.Fatalf("目标目录里残留了文件：%s", e.Name())
		}
	}
}
