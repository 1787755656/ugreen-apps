# ugreen-apps

绿联 UGOS Pro 应用打包 monorepo，参考 [conversun/fnos-apps](https://github.com/conversun/fnos-apps)（飞牛OS同类项目）的 CI 架构改造而来，用 GitHub Actions 自动跟踪各应用的上游新版本、下载/构建、`ugcli` 打包、发布 GitHub Release。

包含：metatube（元数据刮削）、qbittorrent（Enhanced Edition）、natfrp（SakuraFrp 内网穿透客户端）、lucky（网络工具箱：DDNS/反代/端口转发等）、magicpush（多渠道消息推送平台）、picoclaw（Sipeed 超轻量个人 AI Agent）、readeck（开源书签/稍后读）、litepan（网盘聚合挂载 + STRM 刮削）、openlist（AList 接续分支：多存储文件列表）、magicmail（多邮箱 IMAP 代收客户端）。这些原本是桌面上各自独立的手工维护项目，现在合并成一个仓库统一自动化。

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
| picoclaw | `sipeed/picoclaw` | GitHub Releases API，tag 格式 `vX.Y.Z` | 官方静态编译二进制（picoclaw + picoclaw-launcher）直接打包，带 ELF 架构校验；tab 应用直连 launcher 自带 WebUI 管理台（18800）；start.sh 用 `PICOCLAW_HOME` 把全部数据钉到应用 data 目录 + TMPDIR 重定向（沙箱无 /tmp）+ 崩溃循环保护；监听用 `-host 0.0.0.0` 而非 `-public`（沙箱解析不了 localhost，`-public` 会让 launcher 用主机名 localhost 探活 gateway 而全挂，显式 IPv4-any 绑定时探活走字面量 127.0.0.1） |
| ani-rss（显示名 **ass**） | `wushuo894/ani-rss` | GitHub Releases API，tag `vX.Y.Z`，资产 `ani-rss.jar` | 捆绑 Temurin **JRE**（非 JDK）+ Debian `C.utf8` locale（修中文路径）；start.sh + java 包装需 `SYSTEM.EXEC_SYSTEM_COMMAND`；发布 Release 附带上游 changelog |
| readeck | 上游在 Codeberg `readeck/readeck`（GitHub 镜像**无 Releases**，tags 也只同步到 0.3.x） | Codeberg Gitea API `releases/latest`，tag 无 v 前缀（如 `0.22.3`） | Go 单二进制静态编译；`parameters` 的 `type: path` 安装时选数据目录（`READEK_DATA_DIR`），start.sh 兜底应用数据目录；更新说明从 Codeberg 的 `CHANGELOG.md` 按版本号切出（`fetch-changelog-section.sh`）；端口 28180（避开 qBittorrent 的 28080） |
| litepan | `Ponphil/LitePan`（Go 版；上游**无 Release**、tag 停在 v0.3.0-beta，发布渠道是 Docker Hub） | 读 main 分支 `internal/httpx/user_agent.go` 里的 `AppVersion` 常量（如 `v0.4.6-Beta`） | **本仓库唯一需要 Go 工具链现场交叉编译的 app**（magicpush 也从源码构建，但那是 Node）：CI 里下载钉死版本的 Go 工具链交叉编译（前端已由上游预构建并 `go:embed`，不需要 Node）；**不带 `fuse` build tag**（原生沙箱打不开 `/dev/fuse`）；编译前注入 `static/ugos_env.go`（一个只设环境变量的 `init()`，把数据目录/STRM 目录/`TMPDIR` 对到 `UGAPP_*`），上游仓库不打 patch；`parameters` 的 `type: path` 安装时选 STRM 输出目录；端口 **25211**（上游默认的 5211 会和用户自己跑的 Docker 版 LitePan 撞车，真机踩过） |
| openlist | `OpenListTeam/OpenList`（AList 的社区接续分支） | GitHub Releases API，tag `vX.Y.Z`（正好是 project.yaml 要的 x.y.z，无需映射） | 用 **musl 版**资产 `openlist-linux-musl-<arch>.tar.gz`：它是**静态链接**的（默认的 glibc 版是动态链接，`file` 实测确认），沙箱里零运行时依赖最稳；不用 `-lite`（那个把前端剥掉去 CDN 拉）。start.sh：`type: path` 参数选数据目录 + 本机存储目录（顺带授权，启动时逐个自检可访问性并检查父子嵌套）、管理员密码经 `OPENLIST_ADMIN_PASSWORD`/`admin set` 双路径设置（留空则首启生成随机密码写进数据目录的文本文件）、端口/日志/临时目录用 `OPENLIST_*` 环境变量钉死（注意 Scheme 那组**没有子前缀**，是 `OPENLIST_HTTP_PORT` 不是 `OPENLIST_SCHEME_HTTP_PORT`）；端口 **28244**（上游默认 5244，5xxx 段容易和用户自己跑的容器撞） |
| fastnet | KoolCenter 的固件下载服务器目录 `fw.kspeeder.com`（主）/ `fw.koolcenter.com`（备）的 `/binary/fastnet/`，闭源、无 GitHub 仓库、无 Release | 读同目录 `version.txt` 的 `VERSION=` 字段（天然就是 x.y.z，无需映射），带 `no-cache` 头防 CDN 缓存住旧版 | 只打包上游发布的裸二进制 `FastNet-<版本>.<amd64\|arm64>`（armv7 不打，UGOS 没这种机型），按 `version.txt` 里的 sha256 校验；**amd64 那个是 UPX 加壳的、arm64 的不是**，构建时统一解壳（沙箱里自解压要 W→X 未验证 + 加壳是上架审核的"检测规避"特征，见 skill），注意 sha 是加壳文件的所以顺序必须"先校验后解壳"；amd64 那一路在 runner 上真跑一次 `FastNet version` 冒烟。tab 应用直连它自带的 Web 控制台，端口 **28190**；**不启用 `--token`**（加了之后根路径也 401，而 tab 打开时没地方带 token，点开就是个 401 页面）；start.sh 只做 TMPDIR/cwd 重定向后 `exec`（实测跑完整测速一个文件都不落盘） |
| magicmail | `magiccode1412/magicmail` | GitHub Releases API，tag `vX.Y.Z`（正好是 x.y.z，无需映射） | **需要 Go 工具链现场交叉编译**（同 litepan），但**上游没把前端产物提交进仓库**，CI 里要先跑一遍 vite 构建再放到 `server/embedfs/dist`（`go:embed` 嵌空目录不报错，会静默打出前端为空的包 —— build.sh 对此有硬断言）；SQLite 用的是 `glebarez/sqlite`（纯 Go），`CGO_ENABLED=0` 交叉编译成立，产物是单个静态二进制。编译前注入 `static/ugos_env.go`（只设环境变量的 `init()`：把 `--port=` 转成 `MAGICMAIL_PORT`、`chdir` 到数据目录、`MAGICMAIL_DSN`、`TMPDIR`），上游仓库不打 patch；`chdir` 那条是必须的 —— 附件落盘走硬编码的相对路径 `./data/attachments`（三处、无环境变量可覆盖），不切走会写进只读的安装目录。tab 应用（PWA + SSE 长连接 + 附件上传会撞网关的 20MB 上限，都不适合 inner），端口 **23232**（上游默认 8080 被占概率极高） |

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
- **magicpush 钉死 23000 端口**（`project.yaml` 的 `port` 与 start.sh 里的兜底端口一致，改端口时两处要同步）：原本用 3000，为避开其它常占 3000 的应用（如曾打包过的 adguardhome 管理页）改成了高位端口 23000。
- **litepan 的 Go 版本钉死在 `build.sh` 的 `GO_VERSION`**：上游 `go.mod` 要求 go 1.26.4，脚本用 `GOTOOLCHAIN=local` 禁止 go 自己去拉工具链（网络抖动会变成难懂的失败）。上游哪天提高 go.mod 的要求，这里要跟着抬。另外 litepan 的版本探测读的是 main 分支的 `AppVersion` 常量，所以**上游改了内容但没 bump 这个常量不会触发重建**（和 magicpush 同一个取舍）。
- **openlist 的图标用的是官方 logo，而那个 logo 是 CC BY-NC-SA 4.0（非商业）**：素材来自 `OpenListTeam/Logo`，合成脚本在 `scripts/apps/openlist/make-icon.py`。本仓库是免费的社区打包，用它标识应用没问题；但**如果哪天要正式提交到绿联应用中心，这条 NC 条款需要单独确认**（必要时换成自绘图标）。
- **openlist 装好后必须重启一次才能用上所选目录**：这是平台通病（先起服务、2~3 秒后才写参数与授权），不是这个应用的 bug。start.sh 会在日志里逐条打出哪些目录还不可访问，看到就去应用中心「停止 → 启动」。
- **magicmail 装好后要立刻注册管理员账号**：它是 tab 应用，端口对局域网直接可达，而上游的注册接口在"还没有任何用户"时是开放的（之后拒绝）——先注册先得。这是上游设计，Docker 部署也一样，已写进应用描述里。
- **jellyfin / adguardhome / smartdns 暂时移出了仓库**：真机发现问题待排查，项目目录和打包脚本先挪到仓库外保存（`../<应用名>/`），修复后迁回，并把上面表格和 workflow 手动触发说明里的应用列表补回来。


## Release 说明里的上游更新内容

每次 `gh release create` 会：

1. 读 `scripts/apps/<app>/meta.env` 里的 `UPSTREAM_GITHUB=owner/repo`（若有）
2. 用 `scripts/ci/fetch-upstream-notes.sh` 拉取该 tag 的 GitHub Release body
3. 写进本仓库 Release 的 **「上游更新内容」** 一节

没有 GitHub Release 的应用（如 natfrp）会显示占位说明。
