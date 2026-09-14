# UGOS Pro 适配入口。
#
# 上游（xiaocheng154/nas-cloud-downloader）的 app.py 结尾写死了
#     uvicorn.run(app, host="0.0.0.0", port=port)
# 这在绿联上不能直接用：inner 应用的声明端口在局域网上是直接可达的，
# 而上游本身没有任何登录 —— 谁扫到端口谁就能用你的网盘账号。
#
# 所以这里只做一件事：让上游【只监听 127.0.0.1】，对外那一层交给 Go 管理壳
# （鉴权两道闸在 launcher/auth.go，反代在 launcher/main.go）。
#
# 刻意做成一个独立的入口文件而不是去改 app.py：上游怎么升级都不冲突，
# 跟版本时把 src/ 整个换掉、把这个文件放回去就行。
#
# 原作者：xiaocheng154　原项目：https://github.com/xiaocheng154/nas-cloud-downloader
from __future__ import annotations

import os
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
# 上游模块之间是平铺的 `import baidu` 这种写法，必须保证 src 目录在 sys.path 上。
# 直接跑脚本时 sys.path[0] 本来就是它，这里再显式加一次是给"将来入口挪位置"上的保险。
if str(HERE) not in sys.path:
    sys.path.insert(0, str(HERE))

import uvicorn  # noqa: E402  （必须在 sys.path 补好之后再导入）

import app as upstream  # noqa: E402


def main() -> None:
    port = int(os.environ.get("PORT", "28687"))
    uvicorn.run(
        upstream.app,
        host="127.0.0.1",  # ← 整个适配层的核心就是这一行
        port=port,
        # 关掉访问日志：界面在下载时会每秒轮询任务状态，开着的话
        # 应用日志会被刷满，而那个文件是排查启动失败唯一有用的地方。
        # 业务日志上游自己写在 CONFIG_DIR/ 下，界面上能直接看。
        access_log=False,
        # ⚠ 这里【保留】uvicorn 自己的默认日志配置（不要传 log_config=None）：
        # 它把启动和错误打到 stderr，管理壳会把这些行转进应用日志。
        # 传 None 的话 uvicorn 的日志无处可去，"起不来"就变成了一片沉默。
    )


if __name__ == "__main__":
    main()
