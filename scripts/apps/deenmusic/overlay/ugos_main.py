"""绿联 UGOS Pro 启动入口（本移植版新增，不改上游任何业务代码）

上游（Deen 音乐下载器）本身就是标准 Flask 应用，自带 PrefixMiddleware /
url_for / window.APP_BASE 这套"网关前缀"机制。

本应用用绿联的 **tab** 模型（project.yaml: open_type: tab）：桌面点开后在浏览器
【新标签页】里直连本应用自己的端口（http://<NAS>:25400/），整站由 Flask 自己
在根路径提供（/、/static、/api/...），不再经过网关的 /api 反代，也没有 www
静态直出：

    浏览器 ──直连 25400──> 本进程（Flask 根路径）

所以这里把网关前缀设成空（GATEWAY_PREFIX=""）：不启用前缀中间件，Flask 回到
根路径，url_for / 静态资源 / 前端 APP_BASE 都是空前缀。rootfs_common/www 只剩
一个"有人从网关地址打开时"的兜底跳转页。

启动参数（由 Go 管理壳 launcher 注入，也能手工带上）：
    --port <端口>       监听端口，缺省取 UGOS_APP_PORT，再缺省 25400
    --data-dir <目录>   数据目录（等价于上游的 APP_DATA_DIR）
"""

import argparse
import errno
import logging
import os
import sys
from pathlib import Path

logger = logging.getLogger("ugos")

# 只读挂载上会出现的错误码：EROFS 是真正的"只读文件系统"，
# EPERM/EACCES 是"没有权限改"（安装目录属 root，运行用户是 app_xxx）。
_READONLY_ERRNOS = {errno.EROFS, errno.EPERM, errno.EACCES}


def _patch_chmod_for_readonly_install() -> None:
    """让只读安装目录上的 chmod 变成 no-op

    上游三个 bridge 在 spawn API 服务前都会 `bin_path.chmod(0o755)` 兜底，
    但只捕获 `PermissionError`；而绿联沙箱的安装目录是**只读挂载**，chmod 抛的是
    `OSError(EROFS)` —— 异常一路冒到 `_start_api_bridges` 的宽泛 except，
    症状是"三个平台 API 服务启动异常"，主界面正常、搜歌/下载全不可用
    （2026-10-19 真机 0002 实测；/tmp 下测试目录可写，所以本地复现不出来）。

    权限在打包时就是 0755，所以这里把只读文件系统上的 chmod 静默掉；
    其他错误照常抛出，不掩盖真正的问题。
    """
    real_chmod = os.chmod
    if getattr(real_chmod, "_ugos_readonly_patched", False):
        return

    def chmod(path, mode, *args, **kwargs):
        try:
            return real_chmod(path, mode, *args, **kwargs)
        except OSError as exc:
            if exc.errno in _READONLY_ERRNOS:
                logger.debug("只读安装目录，忽略 chmod(%s, 0o%o)", path, mode)
                return None
            raise

    chmod._ugos_readonly_patched = True
    os.chmod = chmod


_patch_chmod_for_readonly_install()

_HERE = Path(__file__).resolve().parent          # <install>/service/webapp
_ROOT = _HERE.parent                             # <install>/service

for _p in (str(_ROOT), str(_HERE)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

# 必须在 import app 之前设置：上游在模块级读它，并据此把 SESSION_COOKIE_PATH
# 收敛到前缀上。tab 模式下没有网关前缀（整站直连本端口），设为空串。
os.environ.setdefault("FNNAS_GATEWAY_PREFIX", "")
GATEWAY_PREFIX = (os.environ.get("FNNAS_GATEWAY_PREFIX") or "").strip().rstrip("/")

# 参数也必须在 import app 之前解析：上游是在模块级就把数据目录（数据库、
# session 密钥、日志）定下来的，晚一步设 APP_DATA_DIR 就完全不起作用。
_parser = argparse.ArgumentParser(add_help=True)
_parser.add_argument("--port", type=int, default=int(os.environ.get("UGOS_APP_PORT") or 25400))
_parser.add_argument("--data-dir", default=None)
_ARGS, _UNKNOWN = _parser.parse_known_args()
if _ARGS.data_dir:
    os.environ["APP_DATA_DIR"] = str(Path(_ARGS.data_dir).expanduser().resolve())

import app as upstream  # noqa: E402  —— 上游入口：建库、注册蓝图、启动任务管理器
from flask import request, session  # noqa: E402
from flask.sessions import SecureCookieSessionInterface  # noqa: E402
from werkzeug.serving import make_server  # noqa: E402

# 会话 Cookie 策略（tab 模式）：
# tab 下应用在浏览器【新标签页】里以"顶层同站"身份打开（http://<NAS>:25400/），
# 不再是跨站 iframe，所以 SameSite 用 Lax 就够（顶层导航会带上）。
# ⚠ 不能用 Secure：直连的是明文 HTTP 端口，带 Secure 的 Cookie 浏览器会拒绝
#   保存/回传，登录会立刻坏掉（这一点和 inner 模式正好相反）。
upstream.app.config["SESSION_COOKIE_SAMESITE"] = "Lax"
upstream.app.config["SESSION_COOKIE_SECURE"] = False

# ---- 不依赖 cookie 的会话通道（token 兜底）----
#
# 绿联网关会给 Set-Cookie 追加 `SameSite=Strict`（应用自己发的是默认 Lax）。
# 一旦用户从"另一个站点"打开本应用（远程访问域名、手机 App、桌面客户端里
# 跨站的窗口），浏览器就不再回传这个 cookie：登录 POST 明明返回 302 成功，
# 紧接着 GET /api/ 却判未登录 → 又回到登录页，怎么登都进不去
# （2026-10-19 真机 0002 实测；curl 同域测是通的，所以只在真实浏览器里复现）。
#
# 这里补一条与 cookie 无关的通道：
#   · 登录/改密成功后，把签名过的会话值放进 302 的 **URL fragment**
#     （fragment 不会发给服务端、也不进 nginx access log），
#     跳到一个静态接收页把它写进 localStorage；
#   · 之后每个页面都注入一段"给 fetch 补 X-Deen-Token 头"的脚本；
#   · 服务端在 session 为空时用这个头恢复会话。
# cookie 正常的环境里这条链只是多一跳，没有任何副作用。
_TOKEN_HEADER = "X-Deen-Token"
_TOKEN_MAX_AGE = 30 * 24 * 3600          # 30 天
_TOKEN_RECEIVER = f"{GATEWAY_PREFIX}/static/deen-token.html"

_TOKEN_SNIPPET = """<script id="deen-token-bridge">
(function(){
  var KEY="deen_token";
  try{
    var h=location.hash||"";
    if(h.indexOf("#t=")===0){
      localStorage.setItem(KEY,h.slice(3));
      history.replaceState(null,"",location.pathname+location.search);
    }
  }catch(e){}
  var t=null; try{ t=localStorage.getItem(KEY); }catch(e){}
  if(!t){ return; }
  var orig=window.fetch;
  if(!orig || orig.__deenWrapped){ return; }
  window.fetch=function(input,init){
    init=init||{};
    try{
      var hs=new Headers(init.headers||{});
      if(!hs.has("__HEADER__")){ hs.set("__HEADER__", t); }
      init.headers=hs;
    }catch(e){}
    return orig.call(this,input,init);
  };
  window.fetch.__deenWrapped=true;
})();
</script>"""


def _install_token_bridge() -> None:
    app = upstream.app
    if getattr(app, "_deen_token_installed", False):
        return
    app._deen_token_installed = True
    serializer = SecureCookieSessionInterface().get_signing_serializer(app)

    @app.before_request
    def _restore_session_from_token():
        if session.get("uid"):
            return None
        token = (request.headers.get(_TOKEN_HEADER) or "").strip()
        if not token:
            return None
        try:
            data = serializer.loads(token, max_age=_TOKEN_MAX_AGE)
        except Exception:  # noqa: BLE001 —— 过期/被改写的 token 直接忽略
            return None
        if isinstance(data, dict) and data.get("uid"):
            session.update(data)
        return None

    @app.after_request
    def _inject_token(resp):
        if not session.get("uid"):
            return resp
        token = serializer.dumps(dict(session))
        # 登录/改密成功的表单提交：把 token 交给静态接收页（fragment 不进日志）
        if (resp.status_code in (301, 302, 303)
                and request.method == "POST"
                and request.path in ("/login", "/change_password")):
            resp.headers["Location"] = f"{_TOKEN_RECEIVER}#t={token}"
            return resp
        # 静态文件（send_file）的响应是 direct_passthrough 流，对它调用 get_data()
        # 会抛 RuntimeError("direct passthrough mode") → 500（真机 0008：登录后
        # 跳 /static/deen-token.html 直接 Internal Server Error）。静态页本来也
        # 不需要注入 token 脚本，直接放行。
        if resp.direct_passthrough:
            return resp
        if resp.mimetype != "text/html":
            return resp
        html = resp.get_data(as_text=True)
        snippet = _TOKEN_SNIPPET.replace("__HEADER__", _TOKEN_HEADER).replace("__TOKEN__", token)
        if "</head>" in html:
            html = html.replace("</head>", snippet + "</head>", 1)
        elif "<body" in html:
            idx = html.index("<body")
            html = html[:idx] + snippet + html[idx:]
        else:
            return resp
        resp.set_data(html)
        return resp


def _retarget_log_file() -> None:
    """把上游的日志文件挪到可写的数据目录

    上游在非 frozen 模式下写 `<service>/logs/webapp.log`，而安装目录在原生沙箱里
    是只读的 —— 结果每次启动只有一行「日志文件初始化失败」，全部内容只能去平台
    日志里翻。这里在 import 之后补一个指向 `data/logs/webapp.log` 的 FileHandler
    （上游那个根本没建成，root 里只有 stdout 那个）。
    """
    data = (os.environ.get("APP_DATA_DIR") or "").strip()
    if not data:
        return
    log_dir = Path(data) / "logs"
    root = logging.getLogger()
    if any(isinstance(h, logging.FileHandler) for h in root.handlers):
        return   # 上游自己建成功了（比如 root 可写），别重复加
    try:
        log_dir.mkdir(parents=True, exist_ok=True)
        handler = logging.FileHandler(log_dir / "webapp.log", encoding="utf-8")
    except OSError as exc:
        logger.warning("日志文件仍落在控制台（%s）：%s", log_dir, exc)
        return
    handler.setFormatter(
        logging.Formatter("%(asctime)s [%(levelname)s] %(name)s - %(message)s")
    )
    root.addHandler(handler)
    logger.info("日志文件：%s", handler.baseFilename)


def _apply_download_dir() -> None:
    """把安装时选的下载目录写进上游的 output_dir（只在"还是默认值"时）

    上游的 output_dir 存在 SQLite 的 settings 表里，默认值 "downloads" 是相对
    路径 —— 在绿联这里会落到只读的安装目录，下载必然失败。所以：
      · 安装参数给了目录 → 首次运行时写进去（之后用户在应用里改了就不动它，
        否则每次重启都会被拉回安装参数，clouddl 上踩过同一个坑）；
      · 没给参数 → 用一个应用数据目录下的绝对路径兜底，至少能下。
    """
    want = (os.environ.get("UGOS_DOWNLOAD_DIR") or "").strip()
    if not want:
        return
    data_dir = (os.environ.get("APP_DATA_DIR") or "").strip()
    try:
        with upstream.app.app_context():
            from models import Setting

            current = (Setting.get("output_dir", "downloads") or "").strip()
            # "还是默认值"或"上次落在应用数据目录里" → 跟随安装参数；
            # 用户自己在应用设置里选过的路径一律不动。
            still_default = current in ("", "downloads") or (
                bool(data_dir) and current.startswith(data_dir)
            )
            if still_default and current != want:
                Setting.set("output_dir", want)
                logger.info("下载目录：%s -> %s", current or "(空)", want)
            else:
                logger.info("下载目录保持用户设置：%s", current)
    except Exception as exc:  # noqa: BLE001 —— 目录偏好写不进去不该拦住启动
        logger.warning("应用下载目录失败（沿用上游设置）：%s", exc)


def _build_server(host: str, port: int):
    """（tab 模式下前缀为空）起 HTTP 服务，直接 serve 上游 Flask app

    用 werkzeug 的 make_server 而不是 app.run()：后者会重新做一次参数解析和
    reloader 判断，而这里要的就是"单进程、多线程、绑 0.0.0.0"。
    """
    wrapped = (
        upstream.PrefixMiddleware(upstream.app, GATEWAY_PREFIX)
        if GATEWAY_PREFIX
        else upstream.app
    )
    return make_server(host, port, wrapped, threaded=True)


def main(argv=None) -> int:
    # 参数已在模块顶部解析过一次（必须早于 import app）；这里只处理重复调用
    args = _ARGS
    if argv:
        args, _unknown = _parser.parse_known_args(argv)
        if args.data_dir:
            os.environ["APP_DATA_DIR"] = str(Path(args.data_dir).expanduser().resolve())

    _retarget_log_file()
    _apply_download_dir()
    _install_token_bridge()

    # tab 模式：浏览器要能从 NAS 的 IP 直连这个端口，必须绑 0.0.0.0（不能只听回环）。
    host, port = "0.0.0.0", args.port

    # 直接复用上游 main()：它负责三件我们不想抄一遍的事 ——
    #   ① 按数据库里的设置（auto_start / 固定端口）拉起三个平台的 API 服务
    #   ② 启动任务管理器（定时同步、下载队列）
    #   ③ 退出时把 bridges / task_manager 干净收尾（finally 段）
    # 我们只把它里面"怎么监听"这件事换掉：_read_web_bind 返回到我们的端口，
    # serve() 换成根路径（空前缀）、绑 0.0.0.0 的实现。
    upstream._read_web_bind = lambda: (host, port)

    def _serve(_app, _gateway_socket, _gateway_prefix, _host, _port):
        try:
            srv = _build_server(host, port)
        except OSError as exc:
            logger.error("监听 %s:%d 失败：%s（端口被占用？）", host, port, exc)
            raise SystemExit(1)
        print(f"[ugos] listening on http://{host}:{port}  prefix={GATEWAY_PREFIX or '(none)'}",
              flush=True)
        try:
            srv.serve_forever()
        except KeyboardInterrupt:
            pass

    upstream.serve = _serve
    upstream.main()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
