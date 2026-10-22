#!/usr/bin/env python3
"""UGTheme — UGOS Pro 主题美化 Docker 应用（Web 服务端，纯 stdlib）。

图标：同名覆盖 /ugreen/static/icons（改前自动备份原图标到数据目录）。
壁纸：登录背景 login/custom.jpg + 桌面壁纸池 bing_YYYYMMDD.jpg。
鉴权：首启设应用密码，PBKDF2 + HMAC 会话 Cookie；除 /api/healthz 外一律要登录。
"""
import hashlib
import hmac
import json
import os
import re
import shutil
import subprocess
import threading
import time
import uuid
import base64
from http import cookies
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import filemgr_patch as fmp
import wallpaper as wp

APP_VERSION = "1.2.2"
PORT = int(os.environ.get("UGTHEME_PORT", "28750"))
DATA_DIR = wp.DATA_DIR
THEME_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "icons")
ICONS_TARGET = os.environ.get("ICONS_DIR", "/ugreen/static/icons")
WEB_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "web")

CONFIG_FILE = os.path.join(DATA_DIR, "config.json")
BACKUP_ICONS = os.path.join(DATA_DIR, "backup", "icons")
BACKUP_DIR = wp.BACKUP_DIR
LOG_FILE = os.path.join(DATA_DIR, "logs", "op.log")

THEMES = [
    {"id": "IOS26", "name": "iOS 26 液态玻璃", "author": "乐小宇 制作"},
    {"id": "SmartisanOS", "name": "锤子OS 风格", "author": "Sunny 整理"},
    {"id": "glass", "name": "拟物毛玻璃", "author": "Sunny 制作"},
    {"id": "ugreenglass", "name": "绿联毛玻璃", "author": "Sunny 制作"},
    {"id": "default", "name": "官方默认（用于还原）", "author": "绿联原厂"},
]
WALLPAPER_PRESETS = [
    {"name": "必应每日（风景）", "url": "https://api.paugram.com/bing"},
    {"name": "二次元·泡图", "url": "https://api.paugram.com/wallpaper/"},
    {"name": "二次元·dmoe", "url": "https://www.dmoe.cc/random.php"},
    {"name": "二次元·loliapi", "url": "https://www.loliapi.com/acg/"},
    {"name": "二次元·xjh", "url": "https://img.xjh.me/random_img.php?return=302"},
    {"name": "二次元妹子·lolicon 高清", "url": "https://api.lolicon.app/setu/v2?r18=0"},
]
CRON_RE = re.compile(
    r"^\S+\s+\S+\s+\S+\s+\S+\s+\S+$")
MAX_UPLOAD = 40 * 1024 * 1024

_lock = threading.Lock()


# ---------- 配置 / 状态 ----------

def load_json(path, default):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return default


def save_json(path, obj):
    d = os.path.dirname(path)
    if d and not os.path.isdir(d):
        os.makedirs(d, exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=1)
    os.replace(tmp, path)


def ensure_dirs():
    for d in (DATA_DIR, BACKUP_ICONS, wp.WALL_CACHE, wp.LOG_DIR):
        os.makedirs(d, exist_ok=True)


def get_config():
    return load_json(CONFIG_FILE, {})


def save_config(cfg):
    save_json(CONFIG_FILE, cfg)


def oplog(msg):
    wp.log(msg)


def seed_crontab():
    """首次启动用环境变量里的 cron 初始化配置；之后配置文件说了算。"""
    cfg = get_config()
    if not cfg.get("cron"):
        cfg["cron"] = os.environ.get("CRON_SCHEDULE") or "0 8 * * *"
        cfg.setdefault("wallpaper_source", wp.DEFAULT_SOURCE)
        save_config(cfg)
        oplog("初始化配置，定时=%s" % cfg["cron"])
    write_crontab(cfg["cron"], strict=False, enabled=cfg.get("wallpaper_enabled", True))


def write_crontab(expr, strict=True, enabled=True):
    if not CRON_RE.match(expr or ""):
        raise ValueError("cron 表达式不合法（需要 5 段）")
    if enabled:
        entry = "%s python3 /app/wallpaper.py >> %s 2>&1\n" % (
            expr, os.path.join(wp.LOG_DIR, "cron.log"))
    else:
        # 总开关关闭：只留注释，crond 不会执行（wallpaper.py 里还有第二道保险）
        entry = "# 壁纸模块已关闭，定时刷新停用（原：%s）\n" % expr
    try:
        os.makedirs("/etc/crontabs", exist_ok=True)
        with open("/etc/crontabs/root", "w") as f:
            f.write(entry)
    except OSError as e:
        if strict:
            raise
        oplog("crontab 写入失败（非容器环境可忽略）：%s" % e)


# ---------- 鉴权 ----------

def hash_password(password, salt):
    return hashlib.pbkdf2_hmac("sha256", password.encode(), bytes.fromhex(salt), 200000).hex()


def setup_password(password):
    if not password or len(password) < 4:
        raise ValueError("密码至少 4 位")
    cfg = get_config()
    if cfg.get("password_hash"):
        raise RuntimeError("已初始化，请直接登录")
    salt = uuid.uuid4().hex
    cfg["salt"] = salt
    cfg["password_hash"] = hash_password(password, salt)
    cfg["session_secret"] = uuid.uuid4().hex + uuid.uuid4().hex
    save_config(cfg)
    oplog("应用密码已设置")


def check_password(password):
    cfg = get_config()
    if not cfg.get("password_hash"):
        return False
    return hmac.compare_digest(hash_password(password, cfg["salt"]), cfg["password_hash"])


def make_token(cfg):
    exp = str(int(time.time()) + 12 * 3600)
    sig = hmac.new(cfg["session_secret"].encode(), exp.encode(), hashlib.sha256).hexdigest()
    return exp + "." + sig


def verify_token(token):
    if not token or "." not in token:
        return False
    exp, sig = token.split(".", 1)
    cfg = get_config()
    if not cfg.get("session_secret"):
        return False
    want = hmac.new(cfg["session_secret"].encode(), exp.encode(), hashlib.sha256).hexdigest()
    if not hmac.compare_digest(sig, want):
        return False
    return int(exp) > time.time()


# ---------- 图标 ----------

def theme_dir(tid):
    if tid not in [t["id"] for t in THEMES]:
        raise ValueError("未知主题")
    return os.path.join(THEME_DIR, tid)


def apply_icons(tid):
    src = theme_dir(tid)
    if not os.path.isdir(src):
        raise RuntimeError("主题目录缺失")
    files = [f for f in os.listdir(src)
             if f.lower().endswith((".png", ".svg", ".jpg", ".jpeg"))]
    replaced, skipped = [], 0
    for fname in files:
        target = os.path.join(ICONS_TARGET, fname)
        if not os.path.exists(target):
            skipped += 1
            continue
        with open(os.path.join(src, fname), "rb") as f:
            data = f.read()
        bak = os.path.join(BACKUP_ICONS, fname)
        if not os.path.exists(bak):
            with open(target, "rb") as f:
                orig = f.read()
            with open(bak, "wb") as f:
                f.write(orig)
        with open(target, "wb") as f:
            f.write(data)
        replaced.append(fname)
    state = load_json(os.path.join(DATA_DIR, "state.json"), {})
    state["last_theme"] = tid
    state["last_theme_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    save_json(os.path.join(DATA_DIR, "state.json"), state)
    oplog("图标主题已应用：%s（替换 %d 个，池外跳过 %d 个）" % (tid, len(replaced), skipped))
    return {"replaced": len(replaced), "skipped": skipped}


def restore_icons():
    if not os.path.isdir(BACKUP_ICONS) or not os.listdir(BACKUP_ICONS):
        raise RuntimeError("没有备份可恢复（从未替换过图标）")
    count = 0
    for fname in os.listdir(BACKUP_ICONS):
        src = os.path.join(BACKUP_ICONS, fname)
        target = os.path.join(ICONS_TARGET, fname)
        if os.path.exists(target):
            with open(src, "rb") as f:
                data = f.read()
            with open(target, "wb") as f:
                f.write(data)
            count += 1
    state = load_json(os.path.join(DATA_DIR, "state.json"), {})
    state["last_theme"] = "restored"
    state["last_theme_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    save_json(os.path.join(DATA_DIR, "state.json"), state)
    oplog("图标已从备份还原 %d 个" % count)
    return {"restored": count}


# 桌面壁纸的读写统一放在 wallpaper.py（定时任务以 CLI 方式跑同一份逻辑），
# 这里只做转发，保证 Web 手动设置与定时自动刷新走的是同一套代码。
def apply_desktop(wall_file):
    return wp.apply_desktop(wall_file)


def restore_desktop():
    return wp.restore_desktop()


# ---------- 状态汇总 ----------

def build_state():
    cfg = get_config()
    state = load_json(os.path.join(DATA_DIR, "state.json"), {})
    samples = {}
    for t in THEMES:
        d = os.path.join(THEME_DIR, t["id"])
        try:
            samples[t["id"]] = sorted(f for f in os.listdir(d)
                                      if f.lower().endswith(".png"))[:4]
        except OSError:
            samples[t["id"]] = []
    pool = []
    try:
        pool = sorted(
            (f for f in os.listdir(wp.WALLPAPER_DIR)
             if f.startswith(wp.POOL_PREFIX) and f.endswith(".jpg")),
            reverse=True)
    except OSError:
        pass
    login_size = 0
    try:
        login_size = os.path.getsize(os.path.join(wp.WALLPAPER_DIR, "login", "custom.jpg"))
    except OSError:
        pass
    icons_backup = len(os.listdir(BACKUP_ICONS)) if os.path.isdir(BACKUP_ICONS) else 0
    return {
        "version": APP_VERSION,
        "setup": not bool(cfg.get("password_hash")),
        "themes": THEMES,
        "theme_samples": samples,
        "last_theme": state.get("last_theme"),
        "last_theme_at": state.get("last_theme_at"),
        "icons_backup_count": icons_backup,
        "login_backup": bool(os.path.exists(os.path.join(BACKUP_DIR, "login_custom.jpg"))),
        "login_size": login_size,
        "last_bing": state.get("last_bing"),
        "login_custom_at": state.get("login_custom_at"),
        "login_restored_at": state.get("login_restored_at"),
        "pool": pool,
        "pool_keep": wp.POOL_KEEP,
        "schedule": cfg.get("cron"),
        "source": cfg.get("wallpaper_source") or wp.DEFAULT_SOURCE,
        "presets": WALLPAPER_PRESETS,
        "wallpaper_enabled": cfg.get("wallpaper_enabled", True),
        "wallpaper_mode": cfg.get("wallpaper_mode") or "both",
        "desktop_users": wp.desktop_users(),
        "last_desktop": state.get("last_desktop"),
        "last_desktop_at": state.get("last_desktop_at"),
        "desktop_restored_at": state.get("desktop_restored_at"),
        "has_desktop_backup": wp.has_desktop_backup(),
        "filemgr": fmp.status(),
    }


def tail_log():
    try:
        with open(LOG_FILE, "r", encoding="utf-8") as f:
            lines = f.readlines()
        return "".join(lines[-60:])
    except OSError:
        return ""


# ---------- HTTP ----------

class Handler(BaseHTTPRequestHandler):
    server_version = "ugtheme"

    def log_message(self, fmt, *args):
        pass

    # --- 基础工具 ---

    def _json(self, obj, code=200, headers=None):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _file(self, path, ctype, cache=False):
        try:
            with open(path, "rb") as f:
                data = f.read()
        except OSError:
            self._json({"error": "not found"}, 404)
            return
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "max-age=300" if cache else "no-store")
        self.end_headers()
        self.wfile.write(data)

    def _session_ok(self):
        c = cookies.SimpleCookie(self.headers.get("Cookie", ""))
        morsel = c.get("ut")
        return bool(morsel and verify_token(morsel.value))

    def _csrf_ok(self):
        return self.headers.get("X-Requested-With") == "fetch"

    def _forbidden(self):
        # code 是给前端认的：只有它代表"会话没了，去登录页"，
        # 登录接口自己的 403（密码错）不带这个 code，前端就不会误报"请先登录"。
        self._json({"error": "登录状态已失效，请重新登录", "code": "unauthorized"}, 403)

    # --- 路由 ---

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/api/healthz":
            self._json({"status": "ok"})
            return
        if path == "/api/welcome":
            # 未登录仅回"是否首次初始化"，不含其它信息
            self._json({"setup": not bool(get_config().get("password_hash"))})
            return
        if path in ("/", "/index.html"):
            # 登录壳本身免鉴权（无任何业务数据，数据全走下面的鉴权 API）；
            # 拦掉它用户打开就只见一坨 JSON，登录表单没机会渲染
            self._file(os.path.join(WEB_DIR, "index.html"), "text/html; charset=utf-8")
            return
        if path in ("/favicon.ico", "/favicon.png"):
            fp = os.path.join(WEB_DIR, "favicon.png")
            if os.path.exists(fp):
                self._file(fp, "image/png", cache=True)
            else:
                self._json({"error": "not found"}, 404)
            return
        if not self._session_ok():
            self._forbidden()
            return
        if path == "/":
            self._file(os.path.join(WEB_DIR, "index.html"), "text/html; charset=utf-8")
        elif path == "/api/state":
            self._json(build_state())
        elif path == "/api/logs":
            self._json({"logs": tail_log()})
        elif path == "/api/preview/wall":
            self._file(os.path.join(wp.WALL_CACHE, "current.jpg"), "image/jpeg")
        elif path == "/api/preview/login":
            self._file(os.path.join(wp.WALLPAPER_DIR, "login", "custom.jpg"), "image/jpeg")
        elif re.match(r"^/api/preview/pool/(bing_\d{8}\.jpg)$", path):
            fname = path.rsplit("/", 1)[1]
            self._file(os.path.join(wp.WALLPAPER_DIR, fname), "image/jpeg")
        else:
            m = re.match(r"^/api/preview/theme/([A-Za-z0-9_]+)/([\w.-]+)$", path)
            if m:
                try:
                    fpath = os.path.join(theme_dir(m.group(1)), m.group(2))
                    if os.path.isfile(fpath):
                        ctype = "image/svg+xml" if fpath.endswith(".svg") else "image/png"
                        self._file(fpath, ctype, cache=True)
                        return
                except Exception:
                    pass
            self._json({"error": "not found"}, 404)

    def do_POST(self):
        path = self.path.split("?")[0]
        if path == "/api/healthz":
            self._json({"status": "ok"})
            return
        if not self._csrf_ok():
            self._json({"error": "bad request"}, 400)
            return
        length = min(int(self.headers.get("Content-Length") or 0), MAX_UPLOAD)
        body = self.rfile.read(length) if length else b""

        try:
            if path in ("/api/setup", "/api/login"):
                req = json.loads(body or b"{}")
                pw = req.get("password", "") or ""
                if path == "/api/setup":
                    setup_password(pw)
                elif not get_config().get("password_hash"):
                    self._json({"error": "还没有设置应用密码，请刷新页面重新设置",
                                "code": "need_setup"}, 400)
                    return
                elif not check_password(pw):
                    self._json({"error": "密码错误，请重新输入", "code": "bad_password"}, 403)
                    return
                self._set_session()
                self._json({"ok": True})
            elif not self._session_ok():
                self._forbidden()
            elif path == "/api/logout":
                self._json({"ok": True},
                           headers={"Set-Cookie": "ut=; Path=/; HttpOnly; Max-Age=0"})
            elif path == "/api/apply_icons":
                req = json.loads(body or b"{}")
                with _lock:
                    result = apply_icons(req.get("theme", ""))
                self._json({"ok": True, **result})
            elif path == "/api/restore_icons":
                with _lock:
                    result = restore_icons()
                self._json({"ok": True, **result})
            elif path == "/api/wallpaper/refresh":
                if not wp.is_enabled():
                    self._json({"error": "壁纸模块已关闭，请先在上方开启"}, 400)
                    return
                with _lock:
                    wp.refresh()
                self._json({"ok": True, "mode": wp.get_mode()})
            elif path == "/api/wallpaper/settings":
                req = json.loads(body or b"{}")
                with _lock:
                    cfg = get_config()
                    if "enabled" in req:
                        cfg["wallpaper_enabled"] = bool(req["enabled"])
                    if "mode" in req:
                        m = req.get("mode")
                        if m not in wp.WALLPAPER_MODES:
                            raise ValueError("同步模式不合法")
                        cfg["wallpaper_mode"] = m
                    save_config(cfg)
                    # 开关一变就重写 crontab，定时任务立刻跟着停/启
                    write_crontab(cfg.get("cron") or "0 8 * * *", strict=False,
                                  enabled=cfg.get("wallpaper_enabled", True))
                oplog("壁纸模块设置：开关=%s 模式=%s" %
                      (cfg.get("wallpaper_enabled", True), cfg.get("wallpaper_mode") or "both"))
                self._json({"ok": True,
                            "enabled": cfg.get("wallpaper_enabled", True),
                            "mode": cfg.get("wallpaper_mode") or "both"})
            elif path == "/api/wallpaper/restore_login":
                with _lock:
                    wp.restore_login()
                self._json({"ok": True})
            elif path == "/api/wallpaper/upload":
                if not wp.is_enabled():
                    self._json({"error": "壁纸模块已关闭，请先在上方开启"}, 400)
                    return
                if not body or not wp.sniff(body):
                    self._json({"error": "仅支持 jpg/png 图片"}, 400)
                    return
                with _lock:
                    wp.set_login_from_file(body)
                self._json({"ok": True})
            elif path == "/api/wallpaper/desktop":
                if not wp.is_enabled():
                    self._json({"error": "壁纸模块已关闭，请先在上方开启"}, 400)
                    return
                req = json.loads(body or b"{}")
                with _lock:
                    result = apply_desktop(req.get("file", ""))
                self._json({"ok": True, **result})
            elif path == "/api/wallpaper/desktop_restore":
                with _lock:
                    result = restore_desktop()
                self._json({"ok": True, **result})
            elif path == "/api/filemgr/apply":
                with _lock:
                    result = fmp.apply()
                self._json({"ok": True, **result})
            elif path == "/api/filemgr/restore":
                with _lock:
                    result = fmp.restore()
                self._json({"ok": True, **result})
            elif path == "/api/filemgr/check":
                with _lock:
                    result = fmp.check()
                self._json({"ok": True, **result})
            elif path == "/api/schedule":
                req = json.loads(body or b"{}")
                expr = (req.get("cron") or "").strip()
                if not CRON_RE.match(expr):
                    self._json({"error": "cron 表达式不合法（需要 5 段，空格分隔）"}, 400)
                    return
                with _lock:
                    cfg = get_config()
                    cfg["cron"] = expr
                    save_config(cfg)
                    write_crontab(expr, strict=False,
                                  enabled=cfg.get("wallpaper_enabled", True))
                oplog("定时刷新已更新：%s" % expr)
                self._json({"ok": True})
            elif path == "/api/source":
                req = json.loads(body or b"{}")
                url = (req.get("url") or "").strip()
                if not re.match(r"^https?://", url):
                    self._json({"error": "URL 必须以 http(s):// 开头"}, 400)
                    return
                cfg = get_config()
                cfg["wallpaper_source"] = url
                save_config(cfg)
                oplog("壁纸源已更新：%s" % url)
                self._json({"ok": True})
            else:
                self._json({"error": "not found"}, 404)
        except (ValueError, RuntimeError) as e:
            self._json({"error": str(e)}, 400)
        except Exception as e:
            oplog("内部错误 %s: %r" % (path, e))
            self._json({"error": "internal error"}, 500)

    def _set_session(self):
        token = make_token(get_config())
        self._json({"ok": True}, headers={
            "Set-Cookie": "ut=%s; Path=/; HttpOnly; SameSite=Lax; Max-Age=43200" % token})


def main():
    ensure_dirs()
    seed_crontab()
    os.makedirs(wp.WALL_CACHE, exist_ok=True)
    httpd = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    oplog("UGTheme v%s 启动，端口 %d" % (APP_VERSION, PORT))
    print("ugtheme v%s listening on %d" % (APP_VERSION, PORT), flush=True)
    httpd.serve_forever()


if __name__ == "__main__":
    main()
