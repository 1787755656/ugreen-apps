"""QQ 音乐 API 的沙箱启动垫片 —— 只补运行环境缺的零件，不改上游代码。

【为什么要有这个文件】（2026-10-22 真机 0012 实测，症状 = 二维码不出来、cookie 登录测试失败）

UGOS 原生应用的 systemd unit 由平台生成，里面有两行硬约束：

    TemporaryFileSystem=/:ro
    BindReadOnlyPaths=/dev

于是**沙箱内的 /dev 是只读的**，/dev/shm 跟着只读。而 Linux 的 POSIX 信号量
就落在 /dev/shm（sem_open 建 /dev/shm/sem.*），所以：

    multiprocessing.SimpleQueue() / Event() / Lock()
        → OSError: [Errno 30] Read-only file system

上游 web/run.py 的 _setup_logging() 给两个 sink 都传了 enqueue=True（loguru 会建
SimpleQueue + Event + Lock），于是 `logger.add()` 在 import 阶段就抛，uvicorn 还没
起来进程就死了。表现：

    [api-shim] QQ 音乐: exec .../run.py
    Traceback ... OSError: [Errno 30] Read-only file system
    （bridge 探活失败 → /api/qq/qr/create 报"无法连接QQ音乐API服务"）

上游在 Windows / fnOS / Docker 上都没有这个限制，所以这不是上游的 bug，是我们的
运行环境少了一块 —— 只能在**我们自己的入口层**把它绕开。

【做法】
不碰上游任何文件：导入 loguru，把 logger.add 包一层强制 enqueue=False（本服务是
单进程单 worker，queue 带来的"异步写盘不阻塞请求"在这里本来就没有收益），然后
runpy 执行原入口 web/run.py，run_name 保持 "__main__"，上游的
`if __name__ == "__main__":` 分支原样生效。

比"构建时 sed 改 run.py"好在：上游升级时不会因为字符串变动而静默失效，
仓库里也没有第二份与上游不同步的 run.py。
"""

import runpy
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent      # <install>/service/apiqq
APP_ROOT = HERE
# run.py 自己也会把 project_root 塞进 sys.path；这里先塞一次是为了让
# `import loguru` 与后面的 `web.src.*` 在 runpy 之前就能解析（顺序无关，幂等）。
if str(APP_ROOT) not in sys.path:
    sys.path.insert(0, str(APP_ROOT))

UPSTREAM_ENTRY = HERE / "web" / "run.py"


def _force_sync_logging() -> None:
    """把 loguru 的异步 sink 掰成同步 —— 沙箱里建不出信号量。"""
    import loguru

    original_add = loguru.logger.add
    if getattr(original_add, "_deen_no_enqueue", False):
        return

    def add_without_enqueue(*args, **kwargs):
        # 无条件覆盖：上游两个 sink 都是 enqueue=True，留一半反而更容易出幻觉
        kwargs["enqueue"] = False
        return original_add(*args, **kwargs)

    add_without_enqueue._deen_no_enqueue = True
    loguru.logger.add = add_without_enqueue


def main() -> None:
    if not UPSTREAM_ENTRY.is_file():
        sys.exit(f"[qqapi-boot] 找不到上游入口 {UPSTREAM_ENTRY}")
    _force_sync_logging()
    runpy.run_path(str(UPSTREAM_ENTRY), run_name="__main__")


if __name__ == "__main__":
    main()
