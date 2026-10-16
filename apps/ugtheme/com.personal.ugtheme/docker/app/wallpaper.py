#!/usr/bin/env python3
"""壁纸下载与应用：登录背景 /ugreen/wallpaper/login/custom.jpg + 桌面壁纸池。

server.py import 本模块做"立即刷新"；crond 定时任务以 CLI 方式运行同一份逻辑。
"""
import json
import os
import subprocess
import time

DATA_DIR = os.environ.get("UGTHEME_DATA", "/data")
WALLPAPER_DIR = os.environ.get("WALLPAPER_DIR", "/ugreen/wallpaper")
DEFAULT_SOURCE = os.environ.get("WALLPAPER_SOURCE", "https://api.paugram.com/bing")
POOL_PREFIX = "bing_"
POOL_KEEP = 14

LOG_DIR = os.path.join(DATA_DIR, "logs")
BACKUP_DIR = os.path.join(DATA_DIR, "backup")
WALL_CACHE = os.path.join(DATA_DIR, "wallpaper")
STATE_FILE = os.path.join(DATA_DIR, "state.json")
CONFIG_FILE = os.path.join(DATA_DIR, "config.json")

MAGIC = ((b"\xff\xd8\xff", "jpg"), (b"\x89PNG", "png"))


def log(msg):
    os.makedirs(LOG_DIR, exist_ok=True)
    line = "[%s] %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    with open(os.path.join(LOG_DIR, "op.log"), "a", encoding="utf-8") as f:
        f.write(line + "\n")


def load_json(path, default):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return default


def save_json(path, obj):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=1)
    os.replace(tmp, path)


def sniff(data):
    for magic, _ext in MAGIC:
        if data[:len(magic)] == magic:
            return True
    return False


def backup_once(src, dst):
    """首次改动前备份，已有备份不覆盖（备份的永远是最原始版本）。"""
    if os.path.exists(src) and not os.path.exists(dst):
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        with open(src, "rb") as f:
            data = f.read()
        with open(dst, "wb") as f:
            f.write(data)
        return True
    return False


def write_bytes_atomic(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.replace(tmp, path)


def _find_image_urls(obj):
    """递归收集 JSON 里所有图片 URL（lolicon 的 urls.original / wallhaven 的 path 等都适用）。"""
    found = []

    def walk(o, key=""):
        if isinstance(o, dict):
            for k, v in o.items():
                walk(v, k)
        elif isinstance(o, list):
            for v in o:
                walk(v, key)
        elif isinstance(o, str) and o.startswith(("http://", "https://")) and \
                "." in o and o.lower().split("?")[0].endswith((".jpg", ".jpeg", ".png", ".webp", ".gif")):
            found.append((key, o))

    walk(obj)
    # 优先原图（键名或路径含 original/full），其次大的
    found.sort(key=lambda kv: (0 if "original" in kv[0].lower() or "original" in kv[1].lower() or "full" in kv[0].lower() else 1))
    return [u for _, u in found]


def _curl(url, timeout):
    r = subprocess.run(["curl", "-fsSL", "--max-time", str(timeout), url],
                       capture_output=True, timeout=timeout + 10)
    return r.stdout if (r.returncode == 0 and r.stdout) else None


def download_wallpaper(url=None, timeout=90):
    """下载并校验，返回字节或抛异常。响应是 JSON 时自动解析出图片 URL 再取一次。"""
    url = url or get_source()
    for attempt in range(3):
        try:
            data = _curl(url, timeout)
            if data and sniff(data):
                return data
            if data:
                try:
                    urls = _find_image_urls(json.loads(data.decode("utf-8", "ignore")))
                except Exception:
                    urls = []
                if urls:
                    pic = _curl(urls[0], timeout)
                    if pic and sniff(pic):
                        return pic
                    log("JSON 源图片下载失败 attempt=%d url=%s" % (attempt, urls[0][:100]))
                else:
                    log("响应既非图片也无可解析图片 URL attempt=%d len=%d" % (attempt, len(data)))
            else:
                log("下载失败 attempt=%d url=%s" % (attempt, url))
        except Exception as e:
            log("下载异常 attempt=%d: %s" % (attempt, e))
        time.sleep(2)
    raise RuntimeError("壁纸下载失败（已重试 3 次）")


def get_source():
    cfg = load_json(CONFIG_FILE, {})
    return cfg.get("wallpaper_source") or DEFAULT_SOURCE


def apply_wallpaper(data, date_str=None):
    """把一张已校验的壁纸写到登录背景 + 桌面壁纸池。返回 (login, pool)。"""
    date_str = date_str or time.strftime("%Y%m%d")
    login_target = os.path.join(WALLPAPER_DIR, "login", "custom.jpg")
    pool_target = os.path.join(WALLPAPER_DIR, POOL_PREFIX + date_str + ".jpg")
    bak = backup_once(login_target, os.path.join(BACKUP_DIR, "login_custom.jpg"))

    write_bytes_atomic(login_target, data)
    write_bytes_atomic(pool_target, data)
    write_bytes_atomic(os.path.join(WALL_CACHE, "current.jpg"), data)

    prune_pool()
    state = load_json(STATE_FILE, {})
    state["last_bing"] = date_str
    state["login_backup"] = True if bak else state.get("login_backup", False)
    save_json(STATE_FILE, state)
    log("壁纸已应用：登录背景 %s + 桌面池 %s (%d bytes)" %
        (login_target, pool_target, len(data)))
    return login_target, pool_target


def prune_pool():
    pool_dir = WALLPAPER_DIR
    try:
        pools = sorted(
            (f for f in os.listdir(pool_dir)
             if f.startswith(POOL_PREFIX) and f.endswith(".jpg")),
            reverse=True)
        for old in pools[POOL_KEEP:]:
            try:
                os.remove(os.path.join(pool_dir, old))
            except OSError:
                pass
    except OSError:
        pass


def restore_login():
    bak = os.path.join(BACKUP_DIR, "login_custom.jpg")
    if not os.path.exists(bak):
        raise RuntimeError("没有可恢复的备份（登录背景从未被改过）")
    target = os.path.join(WALLPAPER_DIR, "login", "custom.jpg")
    with open(bak, "rb") as f:
        write_bytes_atomic(target, f.read())
    state = load_json(STATE_FILE, {})
    state["login_restored_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    save_json(STATE_FILE, state)
    log("登录背景已从备份恢复")
    return target


def set_login_from_file(data):
    """用上传的图片直接设置登录背景（同样先备份）。"""
    login_target = os.path.join(WALLPAPER_DIR, "login", "custom.jpg")
    bak = backup_once(login_target, os.path.join(BACKUP_DIR, "login_custom.jpg"))
    write_bytes_atomic(login_target, data)
    write_bytes_atomic(os.path.join(WALL_CACHE, "current.jpg"), data)
    state = load_json(STATE_FILE, {})
    state["login_backup"] = True if bak else state.get("login_backup", False)
    state["login_custom_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    save_json(STATE_FILE, state)
    log("登录背景已用上传图片更新 (%d bytes)" % len(data))
    return login_target


def refresh(url=None):
    data = download_wallpaper(url)
    login, pool = apply_wallpaper(data)
    return login, pool


if __name__ == "__main__":
    import sys
    os.makedirs(WALL_CACHE, exist_ok=True)
    try:
        refresh()
        print("wallpaper refresh ok")
    except Exception as e:
        log("定时刷新失败: %s" % e)
        print("wallpaper refresh failed: %s" % e, file=sys.stderr)
        sys.exit(1)
