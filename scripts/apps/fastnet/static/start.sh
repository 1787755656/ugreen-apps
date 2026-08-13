#!/bin/sh
# FastNet 启动脚本 (UGOS Pro 原生应用)
# KoolCenter 的全平台网络测速工具，这里只用它的 `web` 子命令：在 NAS 上起一个
# 本地 Web 控制台，用户从浏览器点开就能测延迟/上下行、查 NAT 类型和 IPv6 可用性。
# 由系统以应用独立用户身份运行。
set -u

# ---- 目录准备（全部来自系统环境变量，带兜底） ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
TMP_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/tmp}"

mkdir -p "${DATA_DIR}" "${TMP_DIR}" 2>/dev/null

# 沙箱里没有 /tmp（真机实测，见 ugos-pro-app-dev skill）。FastNet 正常测速时
# 实测一个文件都不落盘（容器里跑完整 quick 体检，docker diff 是空的），但它
# 二进制里确实带着临时文件和 fastnet_test_usage.json 的代码路径 —— 万一哪条
# 被走到，落在不存在的 /tmp 上就是一次谁也看不懂的崩溃。先钉死，几乎零成本。
export TMPDIR="${TMP_DIR}"
export HOME="${HOME:-${DATA_DIR}}"

# 相对路径的状态文件（fastnet_test_usage.json）跟着 cwd 走，切到可写的数据目录，
# 顺带让"卸载保留数据/迁移安装目录"这两件事自动成立。
cd "${DATA_DIR}" || exit 1

# ---- 服务配置 ----
# 监听端口必须与 project.yaml 的 port 一致（供系统探活、桌面图标直连），
# 改动要两处同步。上游默认是"0.0.0.0 随机端口"，随机端口在这里没法用。
# 选 28190：高位冷门端口，避开本仓库其它应用（28080 qbittorrent / 28173
# metatube / 28180 readeck / 28244 openlist）和用户自己可能跑着的容器。
LISTEN_PORT=28190

# --no-open：沙箱里没有浏览器也没有 /usr/bin 下的 xdg-open，上游默认会去尝试
# 打开浏览器（失败只是打一行 "browser opener not available"，但没必要试）。
#
# 不加 --token：加了之后连根路径 `/` 都会 401（真机之外用容器实测：无 token
# 访问 / 和 /webui/ 都是 401，只有 /api/health 放行），而本应用是 open_type: tab
# ——桌面图标只会打开 http://NAS:28190/，没有地方能把 token 带上，用户点开就是
# 一个 401 页面。这个页面本身也不存任何凭据或用户数据，不值得为它牺牲可用性。
exec "${INSTALL_DIR}/bin/fastnet" web --addr "0.0.0.0:${LISTEN_PORT}" --no-open
