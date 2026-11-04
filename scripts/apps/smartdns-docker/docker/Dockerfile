FROM alpine:3.20

# smartdns 官方 linux-all 发行包内容（musl 自包含：二进制 + 相对路径
# interpreter lib/ld-musl-*.so.1 + lib/ 运行库 + smartdns_ui.so 插件），
# 整目录原样保留，由 entrypoint 经 run-smartdns 启动（先 cd 再 exec）。
# LD_LIBRARY_PATH 指向自带 lib 目录：smartdns_ui.so 的 RPATH 上游为空，
# 它对 libgcc_s.so.1 的 dlopen 查找必须显式给路径（UGOS 原生沙箱里
# $ORIGIN 展开不可靠的教训在容器里同样适用，env 是确定性方案）。
ENV LD_LIBRARY_PATH=/app/smartdns/lib
COPY --chmod=755 entrypoint.sh /app/entrypoint.sh
COPY smartdns/ /app/smartdns/
COPY wwwroot/ /app/wwwroot/
COPY smartdns.conf.sample /app/smartdns.conf.sample

WORKDIR /app
EXPOSE 53/udp 53/tcp 6153/tcp
ENTRYPOINT ["/app/entrypoint.sh"]
