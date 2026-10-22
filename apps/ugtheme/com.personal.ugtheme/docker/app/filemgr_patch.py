#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""文件管理「以文本方式打开」——UGOS 前端静态资源补丁。

给 /ugreen/@appstore/com.ugreen.filemgr/www 打四处改动：
  1) 命令枚举加 openAsText
  2) 每条右键菜单数组在「打开」后插入一项
  3) filterFolder 里把「文件夹 / 多选」时的这一项滤掉
  4) 命令分发加一个 case，走 window.CloudWindow_.useCapacity("openViewer",{type:"text"})

**为什么不原地改入口 bundle**：文件管理入口是 `assets/index-<hash>.js`，
服务端给它的缓存头是 `Cache-Control: public, max-age=2592000`，而 PC 客户端
（UGREEN NAS.app）还会把每个应用的静态文件按**文件名**缓存在
`~/Library/UGREEN_Nas_Pro/.cache/<appid>/` 里，重启也不失效——原地改同名文件，
浏览器不硬刷、客户端不清缓存就永远看到旧的。所以这里改成：
   打好的 bundle 另存为 `assets/index-<hash>-ugt<6位md5>.js`，
   再把 `index.html` 里的引用换过去（index.html 是 `no-cache, no-store`，
   两端每次都会重新取）。文件名变了 → 缓存必然 miss → 立刻生效，不用强刷。

三条硬约束（改这里之前先读）：
  * **先备份再写**：backup 落盘失败就一个字节都不写。
  * **全有或全无**：四处锚点全部先算好、全部校验通过才落盘；中途异常自动回滚。
  * **gzip_static 陷阱**：nginx 有同名 .gz 就优先发它、不看 mtime，
    所以凡是写过的文件，同名 .gz 必须同步重建。
"""
import gzip
import hashlib
import json
import os
import re
import time

DATA_DIR = os.environ.get("UGTHEME_DATA", "/data")
WWW = os.environ.get("FILEMGR_WWW", "/ugreen/@appstore/com.ugreen.filemgr/www")
BACKUP_ROOT = os.path.join(DATA_DIR, "backup", "filemgr")
BACKUP_FILES = os.path.join(BACKUP_ROOT, "files")
MANIFEST = os.path.join(BACKUP_ROOT, "manifest.json")

HTML = os.path.join(WWW, "index.html")
MARKER = 'case"openAsText":'
UGT_RE = re.compile(r"-ugt([0-9a-f]{6})\.js$")

# 菜单文案：不依赖语言包（语言包也被客户端按文件名缓存，改了不生效），
# 直接用页面语言，一次写进 bundle，任何端都不会出现"裸 key"。
LABEL_JS = ('/^zh-tw/i.test(navigator.language)?"以文字方式開啟":'
            '/^zh/i.test(navigator.language)?"以文本方式打开":"Open as Text"')


class PatchError(RuntimeError):
    pass


# ---------- 小工具 ----------

def _read(path):
    with open(path, "rb") as f:
        return f.read()


def _md5(data):
    return hashlib.md5(data).hexdigest()


def _write_atomic(path, data):
    tmp = path + ".ugtheme.tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.replace(tmp, path)


def _write_gz(path, data, create=False):
    """同名 .gz 存在就重建（create=True 时没有也建）。

    nginx 开了 gzip_static：有 .gz 就一定发 .gz、不看 mtime，所以只要动过明文，
    同名 .gz 必须同步重建，否则客户端拿到的还是旧内容。
    """
    gz = path + ".gz"
    if not create and not os.path.exists(gz):
        return False
    _write_atomic(gz, gzip.compress(data, 9, mtime=0))
    return True


def _load_json(path, default):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return default


def _save_json(path, obj):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(obj, f, ensure_ascii=False, indent=1)
    os.replace(tmp, path)


def _base_entry(entry):
    """把 `assets/index-X-ugt1a2b3c.js` 还原成官方名 `assets/index-X.js`。"""
    return UGT_RE.sub(".js", entry)


# ---------- 定位 ----------

def _read_html():
    try:
        return _read(HTML).decode("utf-8", "replace")
    except OSError:
        raise PatchError("读不到 %s（文件管理没装，或容器没挂到这个目录）" % HTML)


def _html_entry(html):
    m = re.search(r'src="\./(assets/[^"?]+\.js)"', html)
    if not m:
        raise PatchError("index.html 里找不到入口 bundle 引用")
    return m.group(1)


def entry_info():
    """返回 (index.html 当前引用的入口, 官方入口名, 版本串, html 文本)。"""
    html = _read_html()
    entry = _html_entry(html)
    if not os.path.isfile(os.path.join(WWW, entry)):
        raise PatchError("入口文件不存在：%s" % entry)

    ver = ""
    m = re.search(r"([\w-]+)_(\d+)\(([0-9a-f]+)\)", html)
    if m:
        ver = "%s_%s(%s)" % (m.group(1), m.group(2), m.group(3))
    return entry, _base_entry(entry), ver, html


def _ugt_files():
    """assets 里所有本应用生成的补丁文件（相对路径）。"""
    out = []
    d = os.path.join(WWW, "assets")
    try:
        names = os.listdir(d)
    except OSError:
        return out
    for n in names:
        if UGT_RE.search(n):
            out.append("assets/" + n)
    return out


# ---------- 打补丁（纯计算，不写盘）----------

def _edit_enum(text):
    hits = list(re.finditer(r'(\w+)\.open="open",', text))
    if len(hits) != 1:
        raise PatchError("命令枚举锚点命中 %d 次（期望 1）" % len(hits))
    m = hits[0]
    return m.end(), m.end(), '%s.openAsText="openAsText",' % m.group(1)


def _edit_menus(text):
    hits = list(re.finditer(r'(\w+)\.openItem,', text))
    if len(hits) < 10:
        raise PatchError("菜单锚点只命中 %d 次（期望 ≥10，可能官方改版）" % len(hits))
    item = ('{title:()=>%s,command:"openAsText",disable:!1,'
            'symbol:"icon-pfm-border-file"}' % LABEL_JS)
    return [(m.end(), m.end(), item + ",") for m in hits]


def _brace_end(text, start):
    """从 text[start]=='{' 开始配平，返回匹配的 '}' 下标。跳过字符串/模板/注释。"""
    depth = 0
    i = start
    n = len(text)
    while i < n:
        c = text[i]
        if c in "\"'`":
            q = c
            i += 1
            while i < n:
                if text[i] == "\\":
                    i += 2
                    continue
                if text[i] == q:
                    break
                i += 1
        elif c == "/" and i + 1 < n and text[i + 1] in "/*":
            if text[i + 1] == "/":
                j = text.find("\n", i)
                i = n if j < 0 else j
            else:
                j = text.find("*/", i + 2)
                i = n if j < 0 else j + 1
        elif c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    raise PatchError("花括号未配平（%d 起）" % start)


def _edit_filter(text):
    m = re.search(r"filterFolder\s*\([^)]*\)\s*\{", text)
    if not m:
        raise PatchError("找不到 filterFolder（官方可能改版）")
    body_start = m.end() - 1
    body_end = _brace_end(text, body_start)
    body = text[body_start:body_end + 1]

    sel = re.search(r"selectItems:(\w+)", body)
    ret = re.search(r"return\s+(\w+)\}$", body)
    if not sel or not ret:
        raise PatchError("filterFolder 结构不认识（拿不到 selectItems / return 变量）")
    sel_name, ret_name = sel.group(1), ret.group(1)

    guard = ('if(!(%s.length===1&&%s[0].isFolder===!1))%s=%s.filter('
             'x=>!(x&&x.command==="openAsText"));return %s}'
             % (sel_name, sel_name, ret_name, ret_name, ret_name))
    pos = body_start + ret.start()
    return pos, body_end + 1, guard


def _edit_dispatch(text):
    m = re.search(r'case\s+[\w$.]+\.openCurrentLocal:\{(\w+)\.jump\((\w+)\.selectItemPath\[0\]\);break\}',
                  text)
    if not m:
        raise PatchError("命令分发锚点没命中（找不到 openCurrentLocal 分支）")
    ctx = m.group(2)
    handler = (
        'case"openAsText":{const _ut=%s.selectItems||[],_uf=_ut[0];'
        '_uf&&!_uf.isFolder&&window.CloudWindow_&&window.CloudWindow_.useCapacity('
        '"openViewer",{type:"text",data:{appId:window.ugosAppId||"com.ugreen.filemgr",'
        'path:_uf.path,shareId:Number(_uf.intranetShareId)||0}});break}' % ctx)
    return m.end(), m.end(), handler


def build_patch(text):
    """返回 (新文本, 统计)。任何一处不认识就抛 PatchError，绝不产出半成品。"""
    # 已经有别人的补丁（例如早先手工打的那版）：在它上面再叠一层只会得到重复菜单。
    if "openAsText" in text:
        raise PatchError("这份 bundle 里已经有 openAsText 痕迹，不是干净的官方文件，先还原再打")

    edits = [_edit_enum(text), _edit_filter(text), _edit_dispatch(text)]
    menus = _edit_menus(text)
    edits.extend(menus)

    edits.sort(key=lambda e: (e[0], e[1]))
    for a, b in zip(edits, edits[1:]):
        if a[1] > b[0]:
            raise PatchError("补丁区间重叠，拒绝继续")

    out = text
    for start, end, frag in reversed(edits):
        out = out[:start] + frag + out[end:]

    # 结构自检：命令字面量、菜单项数量、分发分支都要对得上
    if out.count('"openAsText"') != len(menus) + 3:
        raise PatchError("自检失败：openAsText 出现次数不对")
    if out.count(".openItem,{title:()=>") != len(menus):
        raise PatchError("自检失败：注入的菜单项数量不对")
    if MARKER not in out:
        raise PatchError("自检失败：分发分支没进去")
    return out, {"menus": len(menus), "delta": len(out) - len(text)}


# ---------- 备份 ----------

def _backup_manifest():
    return _load_json(MANIFEST, None)


def _rotate_backup():
    if not os.path.isdir(BACKUP_ROOT):
        return
    prev = os.path.join(DATA_DIR, "backup", "filemgr-prev-%s" % time.strftime("%Y%m%d-%H%M%S"))
    try:
        os.replace(BACKUP_ROOT, prev)
    except OSError:
        return


def ensure_backup(rels, base, version, patched_md5=None):
    """按需备份：同一个 file-manager 版本只备份一次原版，之后复用，绝不拿补丁版当原版。"""
    man = _backup_manifest()
    if man and man.get("entry") == base and man.get("files"):
        try:
            cur = _md5(_read(os.path.join(WWW, base)))
        except OSError:
            cur = None
        orig = next((f.get("md5") for f in man["files"] if f.get("rel") == base), None)
        if cur and cur == orig:
            return man, False
        if cur and man.get("patched_md5") and cur == man.get("patched_md5"):
            # 旧方式（原地改 bundle）留下的补丁态：备份里那份才是原版，可以复用
            return man, False

    if man:
        _rotate_backup()

    files = []
    for rel in rels:
        src = os.path.join(WWW, rel)
        if not os.path.isfile(src):
            continue
        data = _read(src)
        dst = os.path.join(BACKUP_FILES, rel)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        _write_atomic(dst, data)
        files.append({"rel": rel, "md5": _md5(data), "size": len(data),
                      "had_gz": os.path.exists(src + ".gz")})
    if not files:
        raise PatchError("没有可备份的文件，中止")

    man = {"created_at": time.strftime("%Y-%m-%d %H:%M:%S"),
           "app_version": version, "entry": base,
           "patched_md5": patched_md5, "files": files, "ugt": None}
    _save_json(MANIFEST, man)
    return man, True


# ---------- 对外：状态 / 打补丁 / 还原 ----------

def status():
    st = {"mounted": os.path.isdir(WWW), "applied": False, "patchable": False,
          "entry": "", "app_version": "", "backup": None, "reason": "",
          "legacy": False, "locales": 0, "backup_stale": False}
    if not st["mounted"]:
        st["reason"] = "没挂到文件管理目录（%s 不存在）" % WWW
        return st
    try:
        entry, base, ver, html = entry_info()
    except PatchError as e:
        st["reason"] = str(e)
        return st

    st["entry"] = entry
    st["app_version"] = ver

    try:
        cur = _read(os.path.join(WWW, entry)).decode("utf-8", "replace")
        if entry != base:
            st["applied"] = MARKER in cur
            st["patchable"] = True
        else:
            if MARKER in cur:
                st["applied"] = True
                st["legacy"] = True
                st["patchable"] = True
                st["reason"] = "检测到 1.2.0 旧方式补丁（原地改 bundle），可直接重新应用升级"
            else:
                build_patch(cur)
                st["patchable"] = True
    except PatchError as e:
        st["reason"] = str(e)
        return st

    man = _backup_manifest()
    if man:
        st["backup"] = {"at": man.get("created_at"), "files": len(man.get("files") or []),
                        "app_version": man.get("app_version")}
        st["backup_stale"] = bool(man.get("entry") != base)
    st["locales"] = 0  # 新方案不再改语言包
    return st


def check():
    """自检：把补丁算一遍但不落盘。"""
    st = status()
    if st["applied"]:
        return {"applied": True, "message": "补丁已生效", **st}
    entry, base, ver, html = entry_info()
    text = _read(os.path.join(WWW, base)).decode("utf-8", "replace")
    out, info = build_patch(text)
    return {"applied": False, "entry": entry, "base": base, "app_version": ver,
            "menus": info["menus"], "delta": info["delta"], "message": "可以打"}


def apply():
    st = status()
    if not st["mounted"]:
        raise PatchError(st["reason"] or "文件管理目录不可用")
    if st["applied"] and not st["legacy"]:
        return {"already": True, "message": "补丁已在生效状态，无需重复应用", **st}
    if st["legacy"]:
        # 先把 1.2.0 那版原地补丁还原掉，再走新方案
        restore()

    entry, base, ver, html = entry_info()
    text = _read(os.path.join(WWW, base)).decode("utf-8", "replace")
    out, info = build_patch(text)
    out_bytes = out.encode("utf-8")
    digest = _md5(out_bytes)
    ugt = base[:-3] + "-ugt%s.js" % digest[:6]

    man, created = ensure_backup([base, "index.html"], base, ver, patched_md5=digest)
    # 备份已落盘，从这里开始才允许写

    try:
        # 清掉上一版留下的补丁文件
        for rel in _ugt_files():
            if rel != ugt:
                for p in (os.path.join(WWW, rel), os.path.join(WWW, rel) + ".gz"):
                    try:
                        os.remove(p)
                    except OSError:
                        pass

        dst = os.path.join(WWW, ugt)
        _write_atomic(dst, out_bytes)
        _write_gz(dst, out_bytes, create=True)   # 新文件名没有旧 .gz，直接建一份

        html_new = html.replace('src="./%s"' % entry, 'src="./%s"' % ugt, 1)
        if ugt not in html_new:
            raise PatchError("改写 index.html 失败（没找到入口引用）")
        _write_atomic(HTML, html_new.encode("utf-8"))
        _write_gz(HTML, html_new.encode("utf-8"))

        # 落盘后复核：新 bundle、.gz、index.html 指向都要对
        back = _read(dst)
        if MARKER.encode() not in back:
            raise PatchError("复核失败：新 bundle 里没有补丁标记")
        if os.path.exists(dst + ".gz"):
            if MARKER.encode() not in gzip.decompress(_read(dst + ".gz")):
                raise PatchError("复核失败：.gz 里没有补丁标记")
        if ugt not in _read_html():
            raise PatchError("复核失败：index.html 没指向新 bundle")
    except Exception:
        try:
            restore()
        except Exception:
            pass
        raise

    man["patched_md5"] = digest
    man["patched_at"] = time.strftime("%Y-%m-%d %H:%M:%S")
    man["ugt"] = ugt
    _save_json(MANIFEST, man)

    st2 = {"at": time.strftime("%Y-%m-%d %H:%M:%S"), "entry": base, "ugt": ugt,
           "app_version": ver, "menus": info["menus"], "locales": 0,
           "backup_created": created, "md5": digest}
    state = _load_json(os.path.join(DATA_DIR, "state.json"), {})
    state["filemgr_patch"] = st2
    _save_json(os.path.join(DATA_DIR, "state.json"), state)
    return {"ok": True, **st2}


def restore():
    man = _backup_manifest()
    if not man or not man.get("files"):
        raise PatchError("没有备份可还原（从未在本应用里打过补丁）")

    n = 0
    for item in man["files"]:
        rel = item["rel"]
        src = os.path.join(BACKUP_FILES, rel)
        if not os.path.isfile(src):
            continue
        data = _read(src)
        dst = os.path.join(WWW, rel)
        _write_atomic(dst, data)
        if item.get("had_gz"):
            _write_gz(dst, data)
        n += 1

    for rel in _ugt_files():
        for p in (os.path.join(WWW, rel), os.path.join(WWW, rel) + ".gz"):
            try:
                os.remove(p)
            except OSError:
                pass

    base = man.get("entry")
    if base and os.path.isfile(os.path.join(WWW, base)):
        if MARKER.encode() in _read(os.path.join(WWW, base)):
            raise PatchError("还原后仍检测到补丁标记，请手工核对")
    if base and base not in _read_html():
        raise PatchError("还原后 index.html 没指回官方入口，请手工核对")

    state = _load_json(os.path.join(DATA_DIR, "state.json"), {})
    state["filemgr_patch"] = {"at": time.strftime("%Y-%m-%d %H:%M:%S"), "restored": True,
                              "entry": base}
    _save_json(os.path.join(DATA_DIR, "state.json"), state)
    return {"ok": True, "restored": n}
