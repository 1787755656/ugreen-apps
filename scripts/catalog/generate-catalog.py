#!/usr/bin/env python3
"""生成商店目录 apps.json。

数据来源（全部以仓库现状为事实）：
  - scripts/apps/<slug>/meta.env        APP_ID / RELEASE_TITLE / HOMEPAGE_URL / SUPPORTED_ARCH / CATALOG_CATEGORY(可选)
  - apps/<slug>/<appid>/project.yaml    tag_types（分类缺省映射）、icon(rootfs_common/icon.png)
  - GitHub Releases（tag 形如 <slug>/vX.Y.Z[-rN]） 资产文件名给出真实的 upk_version（x.y.z.000N）

输出：仓库根 apps.json，字段契约见 ../（ugos-store 仓库）catalog/apps.json 与其 README。
usage:  GITHUB_TOKEN=... python3 scripts/ci/generate-catalog.py   （token 可选，匿名有速率限制）
"""
import json
import os
import re
import sys
import urllib.request
from pathlib import Path

REPO = os.environ.get("GITHUB_REPOSITORY", "1787755656/ugreen-apps")
RAW_BASE = f"https://raw.githubusercontent.com/{REPO}/main"
ASSET_RE = re.compile(r"^(amd64|arm64)_(?:nasync_)?(?P<appid>.+)_(?P<ver>\d+\.\d+\.\d+\.\d{4})\.upk$")

# ugos-store 前端的分类枚举（App.tsx CATEGORIES）；meta.env 可用 CATALOG_CATEGORY 覆盖
TAG_TYPE_MAP = {
    "utility": "system", "system": "system", "devtool": "system",
    "driver": "system", "backup": "system", "security": "system",
    "media": "media", "download": "download",
}


def read_meta(path: Path) -> dict:
    meta = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        v = v.strip().strip('"').strip("'")
        if k in ("APP_ID", "FILE_PREFIX", "RELEASE_TITLE", "HOMEPAGE_URL", "SUPPORTED_ARCH", "CATALOG_CATEGORY"):
            meta[k] = v
    return meta


def read_project(py: Path) -> dict:
    """从 project.yaml 取 tag_types / port；PyYAML 缺席时优雅降级。"""
    out = {"tag_types": [], "port": None}
    try:
        import yaml  # GitHub Actions 的 ubuntu runner 自带
        doc = yaml.safe_load(py.read_text(encoding="utf-8"))
        out["tag_types"] = doc.get("tag_types") or []
        out["port"] = doc.get("port")
        i18n = (doc.get("i18n") or {}).get("zh-CN") or {}
        out["name"] = i18n.get("name")
        out["description"] = (i18n.get("description") or "").strip()
    except Exception as e:  # noqa: BLE001 —— 描述缺失不阻塞目录生成
        print(f"warn: parse {py}: {e}", file=sys.stderr)
    return out


def fetch_releases() -> list:
    url = f"https://api.github.com/repos/{REPO}/releases?per_page=100&page={1}"
    releases, page = [], 1
    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    while url:
        req = urllib.request.Request(url)
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        req.add_header("Accept", "application/vnd.github+json")
        with urllib.request.urlopen(req, timeout=30) as resp:
            batch = json.load(resp)
        releases.extend(batch)
        page += 1
        url = f"https://api.github.com/repos/{REPO}/releases?per_page=100&page={page}" if len(batch) == 100 else None
    return [r for r in releases if not r.get("draft")]


def main() -> None:
    root = Path(__file__).resolve().parents[2]
    releases = fetch_releases()
    by_slug: dict[str, dict] = {}
    for r in sorted(releases, key=lambda x: x.get("published_at") or "", reverse=True):
        slug = r["tag_name"].split("/", 1)[0]
        by_slug.setdefault(slug, r)  # API 已按时间倒序，第一个即最新

    entries, missing = [], []
    for slug_dir in sorted((root / "apps").iterdir()):
        slug = slug_dir.name
        rel = by_slug.get(slug)
        if rel is None:
            missing.append(slug)
            continue
        meta = read_meta(root / "scripts/apps" / slug / "meta.env")
        app_id = meta.get("APP_ID", "")
        if not app_id:
            print(f"warn: {slug} meta.env 缺 APP_ID，跳过", file=sys.stderr)
            continue
        proj = read_project(slug_dir / app_id / "project.yaml")

        urls, upk_version, platforms, downloads = {}, "", [], 0
        for a in rel.get("assets", []):
            m = ASSET_RE.match(a["name"])
            if not m or m.group("appid") != app_id:
                continue
            urls[m.group(1)] = a["browser_download_url"]
            platforms.append(m.group(1))
            upk_version = m.group("ver")
            downloads += a.get("download_count", 0)
        if not urls:
            print(f"warn: {slug} 最新 release 无匹配 {app_id} 的 upk 资产，跳过", file=sys.stderr)
            continue

        icon = slug_dir / app_id / "rootfs_common" / "icon.png"
        tag_types = proj.get("tag_types") or []
        category = meta.get("CATALOG_CATEGORY") or TAG_TYPE_MAP.get(
            tag_types[0] if tag_types else "", "system")

        entries.append({
            "appname": app_id,
            "display_name": meta.get("RELEASE_TITLE") or proj.get("name") or slug,
            "description": proj.get("description", ""),
            "homepage_url": meta.get("HOMEPAGE_URL", ""),
            "updated_at": (rel.get("published_at") or "")[:10],
            "version": re.sub(r"\.\d{4}$", "", upk_version),
            "upk_version": upk_version,
            "release_tag": rel["tag_name"],
            "download_urls": urls,
            "service_port": proj.get("port") or 0,
            "icon_url": f"{RAW_BASE}/apps/{slug}/{app_id}/rootfs_common/icon.png" if icon.exists() else "",
            "download_count": downloads,
            "app_type": "native",
            "category": category,
            "platforms": sorted(set(platforms)),
        })

    out = json.dumps({"apps": entries}, ensure_ascii=False, indent=2) + "\n"
    (root / "apps.json").write_text(out, encoding="utf-8")
    print(f"apps.json: {len(entries)} 个应用" + (f"；无 release: {missing}" if missing else ""))


if __name__ == "__main__":
    main()
