# ugreen-apps

绿联 UGOS Pro 应用打包 monorepo，参考 [conversun/fnos-apps](https://github.com/conversun/fnos-apps)（飞牛OS同类项目）的 CI 架构改造而来，用 GitHub Actions 自动跟踪各应用的上游新版本、下载/构建、`ugcli` 打包、发布 GitHub Release。

包含：metatube（元数据刮削）、qbittorrent（Enhanced Edition）、natfrp（SakuraFrp 内网穿透客户端）、lucky（网络工具箱：DDNS/反代/端口转发等）、magicpush（多渠道消息推送平台）。这些原本是桌面上各自独立的手工维护项目，现在合并成一个仓库统一自动化。

## 目录结构

```
.github/workflows/
  build-apps.yml           # 入口：定时(每天)/手动/push触发，检测哪些app要构建
  reusable-build-app.yml   # 单个app的完整流程：查版本→打包→发布→清理旧版本
scripts/
  ci/resolve-release-tag.sh   # 版本去重 + tag/构建号计算（所有app共用）
  apps/<app>/
    meta.env                  # app_id、项目目录、显示名等元信息
    get-latest-version.sh     # 查上游最新版本
    build.sh                  # 下载/组装二进制到 rootfs_<arch>/
    static/                   # 不随版本变化的文件（start.sh 启动脚本等），由 build.sh 拷贝进 rootfs
apps/<app>/com.xxx.xxx/
  project.yaml
  rootfs_common/{icon.png, www/}
  rootfs_amd64/, rootfs_arm64/   # 由 build.sh 在CI里现场生成，不提交进git（见.gitignore）
```

## 各应用的版本探测方式（已用真实网络请求验证过）

| 应用 | 上游 | 探测方式 | 备注 |
|---|---|---|---|
| metatube | `metatube-community/metatube-server-releases`（不是 sdk-go 源码仓库！这是上游自己发布预编译二进制的仓库） | GitHub Releases API | 已从"本地go build"改成直接下载预编译zip，产物等价 |
| qbittorrent | `c0re100/qBittorrent-Enhanced-Edition` | GitHub Releases API，tag格式 `release-X.Y.Z.W`（4段） | project.yaml 的 version 字段只要前3段（ugcli要求x.y.z），第4段仅用于版本比对去重，不写进project.yaml |
| natfrp | 无版本化URL，无GitHub仓库，`nya.globalslb.net` 的 `/latest/` 目录永远指向最新 | 用 HTTP `Last-Modified` 响应头转成 `YYYY.M.D` 当伪版本号 | 没法用"查最新release"方式探测新版本，细节见 `scripts/apps/natfrp/get-latest-version.sh` 注释 |
| lucky | `gdy666/lucky` | GitHub Releases API，tag 格式 `vX.Y.Z` | 官方静态编译二进制直接打包；start.sh 是收养式守护循环（扛 Lucky 网页里"重启"的自我重启行为）+ TMPDIR 重定向（沙箱无 /tmp） |
| magicpush | `magiccode1412/magicpush` | 上游无 releases 无 tag，读 main 分支 `version.json` 的 `.version` | Node.js 应用：CI 里现场 vite 构建前端、npm 装服务端生产依赖（`--ignore-scripts`）、better-sqlite3 按目标架构直接下载官方预编译 `.node`（带 ELF 架构校验）、捆绑 nodejs.org 官方 Node 20 运行时 |

## 本地验证过什么（不需要真机、不需要GitHub仓库）

开发时已经用真实网络请求逐一跑通并验证：

- 各应用的 `get-latest-version.sh` 都拿到了和当前手工打包版本完全一致的版本号
- 各应用的 `build.sh` 都完整跑通下载/构建，`file` 命令确认产出的二进制架构正确（amd64/arm64 各测过）
- `resolve-release-tag.sh` 用一个临时的本地 git 仓库 + 假 origin + 假 `gh` 命令测试过：新版本/已发布跳过/手动revision递增(-r1→-r2)/手动指定revision 四种场景全部正确
- `ugcli check` / `ugcli pack --arch <单个架构>` 在本地真实跑通过，确认**只有目标架构的 rootfs 需要存在**（另一个架构缺失也不影响打包），这是并行 matrix 按架构分别构建这个设计能成立的关键前提

**没有验证到的**（需要真实 GitHub 仓库之后才能测）：完整 workflow 触发链路本身（`workflow_call`、`needs.*.outputs` 之间的数据传递、`actions/cache`、真实 `gh release create`/`gh release list` 等）。这些 YAML 我按 GitHub Actions 语法写的，本地做了 YAML 语法校验，但没有替代真实跑一次 workflow 的验证。

## 你接下来需要做的事

1. **建仓库并推送**（本地 git 仓库已初始化）：
   ```sh
   cd ~/Desktop/绿联开发/ugreen-apps
   # 去GitHub网页建一个新仓库（比如 ugreen-apps），然后：
   git remote add origin <你的仓库地址>
   git branch -M main
   git push -u origin main
   ```
2. **确认仓库 Settings → Actions → General → Workflow permissions** 里选的是 "Read and write permissions"（`reusable-build-app.yml` 需要建 tag、发 release，用的是默认的 `GITHUB_TOKEN`，权限不够会在 `gh release create` 那步报错）。
3. **先手动触发一次测试**：仓库页面 → Actions → "Build UGOS Pro App Packages" → Run workflow，随便选一个 app（比如 `qbittorrent`，二进制最小、最快）。看它是不是真的建了 tag、发了 release、upk 文件能下载。
4. 确认没问题后，定时任务（每天8点UTC）和 push 触发就会自动跑起来了。

## 已知的遗留问题

- **natfrp 没有 `license_agreement_link`/`source_code_link`**：因为它是第三方商业服务客户端，不是开源代码，`project.yaml` 现状就没配这两项。如果以后要正式上架，需要单独确认合规要求，这不是CI能解决的事。
- **`ugcli` 版本锁定在 1.1.0.13**（`reusable-build-app.yml` 里的 `UGCLI_VERSION`）：故意锁死，避免绿联出新版 `ugcli` 后行为变化影响所有应用的打包，需要升级时手动改这一处。
- **每个 app 的 ugcli `--build` 号**是"这个 app 目前为止发布过多少次 + 1"（见 `resolve-release-tag.sh` 里 `build_num` 的计算方式），跟上游版本号无关，纯粹是为了满足 ugcli 要求"同一版本号下构建号必须递增"这条规则。
- **magicpush 钉死 3000 端口**（`project.yaml` 的 `port` 与 start.sh 里的端口一致）：与其它同样监听 3000 的应用（如曾打包过的 adguardhome）同装会端口冲突，需要改掉其中一个的端口（project.yaml 和 start.sh 一起改）。
- **jellyfin / adguardhome / smartdns 暂时移出了仓库**：真机发现问题待排查，项目目录和打包脚本先挪到仓库外保存（`../<应用名>/`），修复后迁回，并把上面表格和 workflow 手动触发说明里的应用列表补回来。
