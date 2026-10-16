# 主题美化中心（com.personal.ugtheme）打包契约

Docker 型应用。**镜像不在 CI 里构建**，CI 只负责下载镜像 tar、铺 `rootfs_<arch>/images/`、
收窄 `support_arch`，然后由可复用 workflow 统一 `ugcli check` / `ugcli pack` / 发 Release。
（同套路的先例：idphotos。）

## 镜像资产在哪

发布在本仓库 Release，tag 形如 `ugtheme-img/<x.y.z>`，资产两个：

| 资产 | 内容 |
|---|---|
| `img-amd64.tar` | `ugtheme/ugtheme:<x.y.z>` 的 amd64 镜像（`docker save`，OCI-layout） |
| `img-arm64.tar` | 同上，arm64 |

⚠ tag 前缀必须是 `ugtheme-img/`（**不能**是 `ugtheme/`）：`scripts/catalog/generate-catalog.py`
按 tag 的 `/` 前段把 Release 归到应用 slug 上并只取最新的一个，若镜像 Release 与
`ugtheme/vX.Y.Z` 同前缀且发布更晚，生成器会挑到它、发现没有 `.upk` 资产，直接把
这个应用从 `apps.json` 里跳过。

## 本机构建镜像（发布新版本时）

镜像源码在 `apps/ugtheme/com.personal.ugtheme/docker/`（`Dockerfile` + `app/`）：

```bash
cd apps/ugtheme/com.personal.ugtheme
VERSION=1.1.2
# 双架构：本机若是 arm64，amd64 走模拟执行（colima/docker desktop 自带 binfmt）
for ARCH in amd64 arm64; do
  docker build --platform "linux/${ARCH}" --provenance=true -t "ugtheme/ugtheme:${VERSION}" docker/
  docker save -o "/tmp/img-${ARCH}.tar" "ugtheme/ugtheme:${VERSION}"
done
```

## 发布镜像资产 + 让 CI 重打包

```bash
gh release create "ugtheme-img/${VERSION}" --repo 1787755656/ugreen-apps \
  --title "主题美化中心 镜像资产 ${VERSION}" --notes "见 scripts/apps/ugtheme/README.md" \
  /tmp/img-amd64.tar /tmp/img-arm64.tar

# 版本号同源：project.yaml 的 version 提到同一个 x.y.z，push 后 CI 自动重打包
sed -i '' "s/^version:.*/version: ${VERSION}/" apps/ugtheme/com.personal.ugtheme/project.yaml
git commit -am "feat(ugtheme): 版本 ${VERSION}" && git push origin main
```

`compose` 里的 `image: ugtheme/ugtheme:<x.y.z>` 由 `build.sh` 按 `$VERSION` 现场对齐并断言，
仓库里那份保持与 `project.yaml` 同版本即可。

## 构建号（ugcli `--build`）

`ugcli pack --build N` 的 N 由 `scripts/ci/resolve-release-tag.sh` 算：
`max(本应用 tag 总数, 全仓库资产名里出现过的最大构建号 + 1)`。

本应用在本仓库接入前，独立仓库时期已经发到 `1.1.1.0006`（本机手工打包验证过），
所以接入时先推了 7 个计数 tag（`ugtheme/import/0001` … `ugtheme/import/0007`，
照 piggybank 的先例）把构建号垫到 7 —— 否则首包会是 `1.1.1.0001`，
真机上已装的 `0005/0006` 反而算「更新」，商店永远不会提示升级。
