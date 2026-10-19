"""绿联 UGOS Pro 启动入口（本移植版新增，不改上游任何业务代码）

上游（Deen 音乐下载器）本身就是 Flask 应用，而且天然支持"统一网关前缀模式"：
飞牛 fnOS 把 /app/deen-music-downloader/xxx 原样转发进来，模块里的
`PrefixMiddleware` 剥掉前缀写进 SCRIPT_NAME，于是 url_for / 静态资源 /
模板里的链接自动带前缀，前端 `window.APP_BASE = request.script_root` 再让
app.js 的 API 请求也带上它。

绿联的 inner 应用模型是：网关只把 `<proxy_path>/...` 反代给应用，其余路径由
网关从 rootfs_common/www 直接 serve 静态文件。所以这里把前缀设成 `/api`
（= project.yaml 的 proxy_path），整个站点就都活在 `/api/...` 之下：

    浏览器                     网关               本进程（Flask 路由）
    /                       →  www/index.html（跳转页）
    /api/login              →  /login            （页面）
    /api/static/js/app.js   →  /static/js/app.js
    /api/api/songs          →  /api/songs        （上游 API 蓝图挂在 /api 下）

最后一行看着像双写，其实是前缀机制的必然结果：APP_BASE=/api 拼上上游写死的
"/api/songs"。功能上完全等价，且【不需要改上游任何一行代码】。

启动参数（由 Go 管理壳 launcher 注入，也能手工带上）：
    --port <端口>       监听端口，缺省取 UGOS_APP_PORT，再缺省 25400
    --data-dir <目录>   数据目录（等价于上游的 APP_DATA_DIR）
"""

import argparse
import logging
import os
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent          # <install>/service/webapp
_ROOT = _HERE.parent                             # <install>/service

for _p in (str(_ROOT), str(_HERE)):
    if _p not in sys.path:
        sys.path.insert(0, _p)

# 必须在 import app 之前设置：上游在模块级读它，并据此把 SESSION_COOKIE_PATH
# 收敛到前缀上（同域其它应用就读不到这个 cookie 了）。
os.environ.setdefault("FNNAS_GATEWAY_PREFIX", "/api")
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
from werkzeug.serving import make_server  # noqa: E402

logger = logging.getLogger("ugos")


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
    """把上游 Flask app 包一层前缀中间件后起 HTTP 服务

    用 werkzeug 的 make_server 而不是 app.run()：后者会重新做一次参数解析和
    reloader 判断，而这里要的就是"单进程、多线程、只监听回环"。
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

    _apply_download_dir()

    host, port = "127.0.0.1", args.port

    # 直接复用上游 main()：它负责三件我们不想抄一遍的事 ——
    #   ① 按数据库里的设置（auto_start / 固定端口）拉起三个平台的 API 服务
    #   ② 启动任务管理器（定时同步、下载队列）
    #   ③ 退出时把 bridges / task_manager 干净收尾（finally 段）
    # 我们只把它里面"怎么监听"这件事换掉：_read_web_bind 返回到我们的
    # 回环端口，serve() 换成带前缀中间件、只听 127.0.0.1 的实现。
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
