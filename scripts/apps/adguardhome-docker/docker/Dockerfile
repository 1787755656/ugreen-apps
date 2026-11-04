# AdGuard Home UGOS 定制镜像：官方二进制 + 默认配置播种入口
# 全程只有 COPY（无 RUN），跨架构构建不需要 QEMU。
# BASE_IMAGE 由 build.sh --build-arg 注入（adguard/adguardhome:v<上游版本>）。
#
# ⚠ ENTRYPOINT 会重置基础镜像的 CMD 继承（Docker 规范），所以启动参数必须
#   在这里显式重写 CMD——web 端口 3080 也钉在这里（--web-addr），用户在
#   向导/后台改 Web 端口都不会生效；compose 只管端口映射和挂载。
ARG BASE_IMAGE=adguard/adguardhome:v0.107.80
FROM ${BASE_IMAGE} AS src
FROM alpine:3.21
COPY --from=src /opt/adguardhome/AdGuardHome /opt/adguardhome/AdGuardHome
COPY --chmod=755 seed/entrypoint.sh /entrypoint.sh
COPY seed/AdGuardHome.seed.yaml /seed/AdGuardHome.yaml
ENTRYPOINT ["/entrypoint.sh"]
CMD ["--no-check-update", "-c", "/opt/adguardhome/conf/AdGuardHome.yaml", "-w", "/opt/adguardhome/work", "--web-addr", "0.0.0.0:3080"]
