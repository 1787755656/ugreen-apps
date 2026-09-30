#!/usr/bin/env python3
"""把 Redis 官方 Debian 包组装成一个能在绿联原生沙箱里跑起来的自包含目录树。

二进制来自 Redis 官方 apt 仓库（packages.redis.io/deb），那里的 bookworm 分支
amd64 / arm64 都有当前最新的 8.x —— 比 Debian 自带的 redis-server 7.0 新得多，
而且是上游自己构建、自己签名的。

沙箱带来的约束只有一条（比 PostgreSQL 那个应用少两条）：

【/usr 不存在】绿联原生应用的沙箱根目录只有 8 项 —— dev etc lib proc run sys var volume1。
   所以从 glibc 和 ld.so 开始，整条依赖闭包都得自带，运行时用显式 loader 启动：
       ld-linux-<arch>.so.N --library-path <我们的lib> redis-server.bin <配置文件>
   闭包用 elfdeps.py 真解析 DT_NEEDED 得到，不是照记忆列一张表 —— 缺一个 .so 的
   表现是启动即退、只留一行 "No such file or directory"，极难查，
   所以【解析不出闭包就直接构建失败】。

PostgreSQL 那两条这里都不成立，省了很多事：
   - 没有编进二进制的绝对路径要打补丁（Redis 不查 /usr/share 里的任何东西，
     时区走 libc 的 /etc/localtime，而沙箱的 /etc 里恰好有它）；
   - 不需要 sh 包装脚本（Redis 不会 popen 起自己的子进程，
     唯一被 exec 的就是 redis-server 本身，命令行完全由管理壳拼）。
   也因此【不需要打包 redis-cli】：管理壳直接说 RESP 协议（见 launcher/resp.go），
   省掉每次查状态都 fork 一个进程、以及解析它的文本输出。

redis-check-rdb / redis-check-aof 不是独立程序，是 redis-server 自己按 argv[0]
分流的（源码 main() 里 strstr(argv[0], "redis-check-rdb")）。一个同名软链就够 ——
用显式 loader 启动时 argv[0] 就是我们给的那个路径，分流照常生效。
但那个软链【由管理壳在运行时建，不打进包】：ugcli pack 会把软链解引用成
真实文件，实测两个别名各变成一份完整的 4.2MB 拷贝，白白多 8.4MB。

用法:
    python3 fetch-runtime.py --arch arm64 --out <rootfs_arm64/redis>
    python3 fetch-runtime.py --arch arm64 --out ... --redis-version 8.10.0
"""

import argparse
import gzip
import hashlib
import io
import lzma
import os
import re
import shutil
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import elfdeps  # noqa: E402

REDIS_MIRROR = "https://packages.redis.io/deb"
REDIS_SUITE = "bookworm"
DEBIAN_MIRROR = "https://deb.debian.org/debian"
DEBIAN_SECURITY = "https://deb.debian.org/debian-security"
# UGOS Pro 实测是 Debian 12 (bookworm)，运行库照这个版本取，和 redis 的 ~bookworm1 对齐。
DEBIAN_SUITE = "bookworm"

ARCH_ELF = {"amd64": "x86_64", "arm64": "aarch64"}
ARCH_LOADER = {"amd64": "ld-linux-x86-64.so.2", "arm64": "ld-linux-aarch64.so.1"}

# Redis 8 把这四个模块并进了官方发行版，deb 里默认就 loadmodule 加载它们。
# 一起打包，但【是否加载由管理页上的开关决定】：redisearch.so 一个就 15MB，
# 小内存的 NAS 上未必想要。
MODULES = ["redisbloom.so", "redisearch.so", "redistimeseries.so", "rejson.so"]

# soname → Debian 包名。只是"快路径"：查不到的会自动去 Contents 索引里兜底，
# 所以这张表过时了不会打出错误的包，只会慢一点并提示你补上。
SONAME_TO_DEB = {
    "libc.so.6": "libc6", "libm.so.6": "libc6", "libdl.so.2": "libc6",
    "libpthread.so.0": "libc6", "librt.so.1": "libc6", "libresolv.so.2": "libc6",
    "ld-linux-aarch64.so.1": "libc6", "ld-linux-x86-64.so.2": "libc6",
    "libcrypt.so.1": "libcrypt1",
    "libgcc_s.so.1": "libgcc-s1",
    "libstdc++.so.6": "libstdc++6",
    "libssl.so.3": "libssl3", "libcrypto.so.3": "libssl3",
    "libsystemd.so.0": "libsystemd0",
    "libcap.so.2": "libcap2",
    "libgcrypt.so.20": "libgcrypt20",
    "libgpg-error.so.0": "libgpg-error0",
    "liblzma.so.5": "liblzma5",
    "libzstd.so.1": "libzstd1",
    "liblz4.so.1": "liblz4-1",
}


def _mirror_key(mirror):
    """把镜像地址压成一个能进文件名的短标识。

    ⚠ 索引的缓存文件名【必须】带上镜像，不能只用 suite ——
    packages.redis.io 和 deb.debian.org 的 suite 都叫 bookworm，只用 suite 做键
    会让第二个仓库的索引直接命中第一个的缓存文件。表现极其难查：
    索引"加载成功、0 个新包"，然后在解析依赖闭包时报
    "无法为 soname libcrypt.so.1 找到 Debian 包" —— 看起来像上游换了依赖，
    其实是 Debian 的索引根本没被下载下来。（第一次跑就踩了。）
    """
    return re.sub(r"[^a-z0-9]+", "-", mirror.lower().split("//", 1)[-1]).strip("-")


class NotFound(Exception):
    """远端没有这个 URL（HTTP 4xx）。和"网络出问题"分开，因为前者不该重试。"""


def log(msg):
    print("[fetch-runtime] %s" % msg, flush=True)


def _check(path, size, sha256):
    """校验已落地的文件。size/sha256 为 None 时跳过对应的那一项。"""
    if size is not None and os.path.getsize(path) != size:
        return "大小是 %d，索引说应为 %d" % (os.path.getsize(path), size)
    if sha256:
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
        if h.hexdigest() != sha256:
            return "SHA256 不符"
    return None


def download(url, dest, size=None, sha256=None, tries=3):
    """下载并【校验】。

    必须校验：HTTP 连接中途断掉时 copyfileobj 只是少写几个字节，不抛任何异常 ——
    于是缓存里留下一个看起来正常的半截文件，然后在很后面的某一步炸出一个完全
    指错方向的错。Debian / Redis 的 Packages 索引本来就带 Size 和 SHA256，
    白给的东西没理由不用。
    """
    if os.path.exists(dest) and os.path.getsize(dest) > 0:
        bad = _check(dest, size, sha256)
        if bad is None:
            return dest
        log("::warning:: 缓存里的 %s %s，重新下载" % (os.path.basename(dest), bad))
        os.remove(dest)

    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    last = None
    for attempt in range(1, tries + 1):
        log("下载 %s%s" % (url, "" if attempt == 1 else "（第 %d 次尝试）" % attempt))
        try:
            _fetch(url, tmp)
        except NotFound:
            # 4xx 不重试，直接往上抛：调用方正靠它做"先试 .gz 再试 .xz"这类探测。
            if os.path.exists(tmp):
                os.remove(tmp)
            raise
        except Exception as exc:
            last = "传输失败：%s" % exc
            log("::warning:: %s" % last)
            continue
        last = _check(tmp, size, sha256)
        if last is None:
            os.rename(tmp, dest)
            return dest
        log("::warning:: 下载校验失败：%s" % last)
    if os.path.exists(tmp):
        os.remove(tmp)
    raise SystemExit("%s 下载 %d 次都没通过校验（%s）" % (url, tries, last))


def _fetch(url, tmp):
    """真正搬字节的那一步。优先用 curl。

    为什么不直接用 urllib：实测 deb.debian.org 上的某些文件用 urllib 下会【静默截断】，
    而同一时刻 curl 拉同一个 URL 每次都完整。curl 自带重试、超时分级和断点续传。
    urllib 留作没有 curl 时的兜底。
    """
    if shutil.which("curl"):
        # 【直接读 HTTP 状态码，不靠 curl 的退出码判断 404】：
        # 实测 deb.debian.org 的 404 让 curl 退出码是 56 而不是文档里说的 22，
        # 靠退出码分类会把"没这个文件"误判成"网络故障"，
        # 于是 download_index 的 ".gz 不行就试 .xz" 探测就废了。
        r = subprocess.run(
            ["curl", "-sSL", "--retry", "3",
             "--connect-timeout", "30", "--max-time", "900",
             "-A", "redis-ugreen-app/build",
             "-w", "%{http_code}", "-o", tmp, url],
            capture_output=True)
        code = r.stdout.decode("ascii", "replace").strip()[-3:]
        if code.startswith("4"):
            raise NotFound("%s → HTTP %s" % (url, code))
        if r.returncode != 0 or not code.startswith("2"):
            raise RuntimeError("curl 退出码 %d，HTTP %s：%s" % (
                r.returncode, code or "?", r.stderr.decode("utf-8", "replace").strip()))
        return
    req = urllib.request.Request(url, headers={"User-Agent": "redis-ugreen-app/build"})
    try:
        with urllib.request.urlopen(req, timeout=180) as resp, open(tmp, "wb") as f:
            shutil.copyfileobj(resp, f)
    except urllib.error.HTTPError as exc:
        raise NotFound(url) from exc


def download_index(url_base, dest_base):
    """下载索引并解压成文本。压缩格式各仓库不一致（security 只有 .xz），逐个试。"""
    last_err = None
    for suffix, opener in ((".gz", gzip.open), (".xz", lzma.open)):
        try:
            path = download(url_base + suffix, dest_base + suffix)
        except NotFound as e:
            last_err = e
            continue
        return opener(path, "rt", encoding="utf-8", errors="replace")
    # packages.redis.io 的 Packages 是【不压缩】的纯文本，最后再试一次裸文件。
    try:
        path = download(url_base, dest_base)
    except NotFound as e:
        raise last_err or e
    return open(path, "rt", encoding="utf-8", errors="replace")


def ar_members(path):
    """解析 ar 归档，产出 (成员名, 数据)。

    自己解析而不是调系统 ar，图的是不依赖平台上有什么、以及报错能指对地方：
    macOS 的 BSD ar 遇到问题只会说一句 "Inappropriate file type or format"。
    ar 格式本身简单到不值得为它引入平台依赖。
    """
    with open(path, "rb") as f:
        blob = f.read()
    if not blob.startswith(b"!<arch>\n"):
        raise RuntimeError("%s 不是 ar 归档" % path)
    pos = 8
    longnames = b""
    while pos + 60 <= len(blob):
        hdr = blob[pos:pos + 60]
        if hdr[58:60] != b"`\n":
            raise RuntimeError("%s 偏移 %d 处的成员头损坏" % (path, pos))
        name = hdr[0:16].decode("ascii", "replace").rstrip()
        size = int(hdr[48:58].decode("ascii").strip())
        pos += 60
        data = blob[pos:pos + size]
        pos += size + (size & 1)          # 数据按偶数字节对齐

        if name == "//":                  # GNU 长文件名表，本身不是成员
            longnames = data
            continue
        if name.startswith("/") and name[1:].isdigit():
            off = int(name[1:])
            end = longnames.find(b"/\n", off)
            name = longnames[off:end].decode("ascii", "replace")
        elif name.startswith("#1/"):      # BSD 长名：名字存在数据区开头
            n = int(name[3:])
            name = data[:n].split(b"\0")[0].decode("ascii", "replace")
            data = data[n:]
        yield name.rstrip("/"), data


def extract_deb(deb_path, dest_dir):
    """解开 .deb（ar 归档里的 data.tar.{xz,zst,gz}）到 dest_dir。"""
    os.makedirs(dest_dir, exist_ok=True)
    data = next((blob for name, blob in ar_members(deb_path)
                 if name.startswith("data.tar")), None)
    if data is None:
        raise RuntimeError("%s 里没有 data.tar.*" % deb_path)
    if data[:4] == b"\x28\xb5\x2f\xfd":
        # tarfile 不认 zstd，交给外部 zstd。目前都是 xz，这只是兜底。
        data = subprocess.run(["zstd", "-dc"], check=True, input=data,
                              stdout=subprocess.PIPE).stdout
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:   # 自动识别 xz/gz/bz2
        tf.extractall(dest_dir)


class AptIndex:
    """一个 apt 仓库的 Packages 索引（可叠加多个仓库，后加载的覆盖先加载的）。"""

    def __init__(self, arch, cache):
        self.arch = arch
        self.cache = cache
        self.packages = {}      # name → (url, version, size, sha)
        self._contents = None

    @staticmethod
    def _vkey(version):
        """dpkg 风格的版本比较键：按"数字段 / 非数字段"交替切开，数字段按数值比。

        必须这么比而不能直接比字符串：redis 的索引里【同一个包有几十个版本条目】
        （从 6.x 一路到 8.10），字符串序会把 8.9 排在 8.10 后面（'9' > '1'），
        于是"最新版"会选错。这个坑很隐蔽 —— 打出来的包能用，只是悄悄落后一截。
        """
        parts = re.findall(r"(\d+)|(\D+)", version or "")
        return [(0, int(d)) if d else (1, s) for d, s in parts]

    def add(self, mirror, suite, required=True, component="main"):
        url_base = "%s/dists/%s/%s/binary-%s/Packages" % (mirror, suite, component, self.arch)
        dest_base = os.path.join(self.cache, "idx", "%s_%s_%s_%s_Packages" % (
            _mirror_key(mirror), suite.replace("/", "_"), component, self.arch))
        try:
            handle = download_index(url_base, dest_base)
        except Exception as exc:
            if required:
                raise
            # 拿不到不该让整个构建失败，但必须显式喊出来，不能静默降级。
            log("::warning:: 取不到 %s 索引（%s）" % (suite, exc))
            return
        n = 0
        with handle as f:
            name = filename = version = None
            size = sha = None
            for line in f:
                line = line.rstrip("\n")
                if not line:
                    if name and filename:
                        n += self._offer(name, "%s/%s" % (mirror, filename),
                                         version, size, sha)
                    name = filename = version = None
                    size = sha = None
                elif line.startswith("Package: "):
                    name = line[9:]
                elif line.startswith("Filename: "):
                    filename = line[10:]
                elif line.startswith("Version: "):
                    version = line[9:]
                elif line.startswith("Size: "):
                    size = int(line[6:])
                elif line.startswith("SHA256: "):
                    sha = line[8:]
            if name and filename:
                n += self._offer(name, "%s/%s" % (mirror, filename), version, size, sha)
        log("已加载 %s/%s 索引：%d 个包" % (suite, component, n))

    def _offer(self, name, url, version, size=None, sha=None):
        """收下一个候选，只保留版本最高的那个（索引里同名多版是常态）。"""
        cur = self.packages.get(name)
        if cur is None or self._vkey(version) > self._vkey(cur[1]):
            self.packages[name] = (url, version, size, sha)
            return 1
        return 0

    def all_versions(self, mirror, suite, pkg, component="main"):
        """列出某个包的全部版本（用于 --redis-version 精确取版）。"""
        url_base = "%s/dists/%s/%s/binary-%s/Packages" % (mirror, suite, component, self.arch)
        dest_base = os.path.join(self.cache, "idx", "%s_%s_%s_%s_Packages" % (
            _mirror_key(mirror), suite.replace("/", "_"), component, self.arch))
        out = {}
        with download_index(url_base, dest_base) as f:
            cur = {}
            for line in f:
                line = line.rstrip("\n")
                if not line:
                    if cur.get("Package") == pkg and "Filename" in cur:
                        out[cur["Version"]] = ("%s/%s" % (mirror, cur["Filename"]),
                                               cur["Version"],
                                               int(cur.get("Size", 0)) or None,
                                               cur.get("SHA256"))
                    cur = {}
                elif ": " in line:
                    k, v = line.split(": ", 1)
                    cur[k] = v
            if cur.get("Package") == pkg and "Filename" in cur:
                out[cur["Version"]] = ("%s/%s" % (mirror, cur["Filename"]), cur["Version"],
                                       int(cur.get("Size", 0)) or None, cur.get("SHA256"))
        return out

    def _load_contents(self):
        """Contents 索引把文件路径映射到包名，用来兜底解析未知 soname（约 40MB，按需下载）。"""
        if self._contents is not None:
            return
        self._contents = {}
        url_base = "%s/dists/%s/main/Contents-%s" % (DEBIAN_MIRROR, DEBIAN_SUITE, self.arch)
        dest_base = os.path.join(self.cache, "idx", "Contents-%s" % self.arch)
        with download_index(url_base, dest_base) as f:
            for line in f:
                if ".so" not in line:
                    continue
                parts = line.rsplit(None, 1)
                if len(parts) != 2:
                    continue
                base = os.path.basename(parts[0].strip())
                if ".so" not in base:
                    continue
                self._contents.setdefault(base, parts[1].strip().split(",")[0].split("/")[-1])
        log("已加载 Contents 索引：%d 个 .so 条目" % len(self._contents))

    def package_for_soname(self, soname):
        pkg = SONAME_TO_DEB.get(soname)
        if pkg and pkg in self.packages:
            return pkg, False
        self._load_contents()
        pkg = self._contents.get(soname)
        if pkg and pkg in self.packages:
            return pkg, True          # True = 表里没有、靠 Contents 查出来的
        return None, False

    def fetch(self, pkg, stage):
        return self.fetch_entry(self.packages[pkg], stage)

    def fetch_entry(self, entry, stage):
        url, version, size, sha = entry
        deb = download(url, os.path.join(self.cache, "deb", os.path.basename(url)),
                       size=size, sha256=sha)
        extract_deb(deb, stage)
        with open(os.path.join(stage, ".extracted"), "w") as f:
            f.write(version + "\n")
        return version


def staged(stage):
    """这个 stage 目录是不是【解包成功】过。

    不能只判断目录在不在：解包失败时目录已经建出来了，只是空的，下次跑就会被
    当成有效缓存直接跳过，然后在很靠后的地方报一个完全指错方向的错。
    """
    return os.path.exists(os.path.join(stage, ".extracted"))


def install_soname(src_roots, lib_dir, soname, elf_arch):
    """从 src_roots 里找出 SONAME 等于 soname 的那一个共享库，装进 lib_dir。

    【只装点名要的那一个】，不是把整棵树里的 .so 一扫而空 —— 后者的代价是
    libc6 的 deb 里几百个 gconv 编码模块会全被扫进来。

    落地时按 SONAME 命名并平铺到一个目录（ld.so 的 --library-path 只认目录列表），
    符号链接一律解引用成真实文件，不去赌 upk 打包对链接的处理。
    """
    os.makedirs(lib_dir, exist_ok=True)
    for root in src_roots:
        if not os.path.exists(root):
            continue
        for path, elf in elfdeps.iter_elf([root]):
            if elf.arch != elf_arch or elf.e_type not in (2, 3):
                continue
            if (elf.soname() or os.path.basename(path)) != soname:
                continue
            dest = os.path.join(lib_dir, soname)
            shutil.copy2(path, dest)
            os.chmod(dest, 0o755)
            return True
    return False


def resolve_closure(out_dir, elf_arch, index, cache, max_rounds=10):
    """反复解析 DT_NEEDED 闭包，缺什么补什么，直到不再缺。"""
    lib_dir = os.path.join(out_dir, "lib", "runtime")
    roots = [os.path.join(out_dir, "bin"), os.path.join(out_dir, "modules")]
    roots = [r for r in roots if os.path.exists(r)]
    auto_found = {}

    for rnd in range(1, max_rounds + 1):
        resolved, missing = elfdeps.closure([lib_dir], roots)
        if not missing:
            log("闭包解析完成：%d 个共享库，无缺口（第 %d 轮）" % (len(resolved), rnd))
            return resolved, auto_found
        log("第 %d 轮：还缺 %d 个 —— %s" % (rnd, len(missing), ", ".join(sorted(missing))))
        progressed = False
        for soname in sorted(missing):
            pkg, from_contents = index.package_for_soname(soname)
            if pkg is None:
                raise SystemExit(
                    "无法为 soname %s 找到 Debian 包。\n"
                    "  它既不在 SONAME_TO_DEB 表里，Contents 索引里也查不到。\n"
                    "  多半是上游换了依赖：手动确认后加进 SONAME_TO_DEB 即可。" % soname)
            if from_contents:
                auto_found[soname] = pkg
            stage = os.path.join(cache, "stage-deb", "%s-%s" % (pkg, index.arch))
            if not staged(stage):
                index.fetch(pkg, stage)
            if not install_soname([stage], lib_dir, soname, elf_arch):
                raise SystemExit(
                    "Debian 包 %s 里没有 soname 为 %s 的库 —— "
                    "soname→包的映射错了，检查 SONAME_TO_DEB" % (pkg, soname))
            log("  %s ← %s" % (soname, pkg))
            progressed = True
        if not progressed:
            raise SystemExit("闭包无法收敛：这一轮一个新库都没装上，检查上面的日志")
    raise SystemExit("闭包在 %d 轮内没有收敛" % max_rounds)


# redis-check-rdb / redis-check-aof 不是独立程序，而是 redis-server 按 argv[0] 分流
# （源码 main() 里 strstr(argv[0], "redis-check-rdb")）。我们用显式 loader 启动，
# argv[0] 就是给 loader 的那个路径，所以只要有一个同名的软链就够了。
#
# ⚠ 但那个软链【不能打进包里】：ugcli pack 会把软链解引用成真实文件
# （实测：两个 4.2MB 的 .bin 各变成一份完整拷贝，白白多 8.4MB）。
# 所以改成由管理壳在数据目录里【运行时建】，见 launcher/redis.go 的 ensureBinAliases。


def assemble(stage, out_dir, version):
    """从解包出来的 Debian 树里挑出要的东西，组装成最终布局。

    布局是我们自己定的（Redis 不像 PostgreSQL 那样按可执行文件路径反推 share 目录，
    想放哪儿放哪儿）：

        redis/
        ├── bin/redis-server.bin        真正的 ELF
        ├── bin/redis-check-rdb.bin →   软链，靠 argv[0] 分流
        ├── lib/runtime/                依赖闭包 + loader（平铺，给 --library-path）
        ├── modules/                    Redis 8 自带的四个模块
        └── VERSION                     打包时的上游版本号
    """
    bin_dir = os.path.join(out_dir, "bin")
    mod_dir = os.path.join(out_dir, "modules")
    os.makedirs(bin_dir, exist_ok=True)
    os.makedirs(mod_dir, exist_ok=True)

    src = os.path.join(stage, "usr", "bin", "redis-server")
    if not os.path.exists(src):
        raise SystemExit("解包出来的树里没有 usr/bin/redis-server —— 上游改布局了？")
    dst = os.path.join(bin_dir, "redis-server.bin")
    shutil.copy2(src, dst)
    os.chmod(dst, 0o755)

    src_mod = os.path.join(stage, "usr", "lib", "redis", "modules")
    found = []
    for name in MODULES:
        p = os.path.join(src_mod, name)
        if not os.path.exists(p):
            log("::warning:: 上游没有模块 %s，跳过" % name)
            continue
        shutil.copy2(p, os.path.join(mod_dir, name))
        os.chmod(os.path.join(mod_dir, name), 0o755)
        found.append(name)
    if not found:
        raise SystemExit("一个模块都没找到 —— 上游把 modules 挪走了？检查 deb 内容")

    with open(os.path.join(out_dir, "VERSION"), "w") as f:
        f.write(version + "\n")
    log("组装完成：redis-server + %d 个模块（版本 %s）" % (len(found), version))
    return found


def verify(out_dir, arch, modules):
    """构建期自检。每一条都对应一种"装到 NAS 上才会发现"的失败。"""
    elf_arch = ARCH_ELF[arch]
    loader = ARCH_LOADER[arch]
    problems = []

    def bad(msg):
        problems.append(msg)

    server = os.path.join(out_dir, "bin", "redis-server.bin")
    if not os.path.exists(server):
        bad("bin/redis-server.bin 不存在")
    else:
        try:
            _, elf = next(iter(elfdeps.iter_elf([server])))
            if elf.arch != elf_arch:
                bad("redis-server.bin 的架构是 %s，应为 %s" % (elf.arch, elf_arch))
        except StopIteration:
            bad("redis-server.bin 不是 ELF")

    # loader 必须在，而且架构要对 —— 拿错架构的 loader 会报
    # "cannot execute binary file"，和"没打包"完全是两种表现，分不清就很难查。
    ld = os.path.join(out_dir, "lib", "runtime", loader)
    if not os.path.exists(ld):
        bad("缺少 loader %s" % loader)

    # 闭包不能有缺口。缺一个 .so 的表现是启动即退、只有一行 No such file or directory。
    lib_dir = os.path.join(out_dir, "lib", "runtime")
    roots = [os.path.join(out_dir, "bin"), os.path.join(out_dir, "modules")]
    _, missing = elfdeps.closure([lib_dir], [r for r in roots if os.path.exists(r)])
    if missing:
        bad("依赖闭包still缺 %s" % ", ".join(sorted(missing)))

    # 架构混装是最难查的一类：包能装上、能启动，只是在另一台机器上跑不了。
    for root, _dirs, files in os.walk(lib_dir):
        for name in files:
            p = os.path.join(root, name)
            for _, elf in elfdeps.iter_elf([p]):
                if elf.arch != elf_arch:
                    bad("lib/runtime/%s 的架构是 %s，应为 %s" % (name, elf.arch, elf_arch))

    for name in modules:
        p = os.path.join(out_dir, "modules", name)
        if not os.path.exists(p):
            bad("模块 %s 不见了" % name)

    # 包里【只该有】redis-server.bin 这一个可执行文件。
    # redis-check-* 的软链由管理壳在运行时建（打进包会被 ugcli 解引用成完整拷贝）。
    extra = [n for n in os.listdir(os.path.join(out_dir, "bin")) if n != "redis-server.bin"]
    if extra:
        bad("bin/ 里多了 %s —— argv[0] 软链应该由管理壳在运行时建，不要打进包" % ", ".join(extra))

    # 版本号文件，管理页要显示
    vf = os.path.join(out_dir, "VERSION")
    if not os.path.exists(vf) or not open(vf).read().strip():
        bad("VERSION 文件缺失或为空")

    if problems:
        for p in problems:
            log("::error:: %s" % p)
        raise SystemExit("构建自检没通过（%d 项）" % len(problems))
    log("构建自检通过：架构一致 / loader 就位 / 闭包无缺口 / %d 个模块 / bin 目录干净"
        % len(modules))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arch", required=True, choices=sorted(ARCH_ELF))
    ap.add_argument("--out", required=True, help="输出目录，例如 rootfs_arm64/redis")
    ap.add_argument("--redis-version", default="",
                    help="上游版本号，例如 8.10.0。留空 = 取索引里最新的")
    ap.add_argument("--cache", default=os.path.join(
        os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "cache"))
    args = ap.parse_args()

    out_dir = os.path.abspath(args.out)
    if os.path.exists(out_dir):
        shutil.rmtree(out_dir)
    os.makedirs(out_dir)

    index = AptIndex(args.arch, args.cache)
    index.add(REDIS_MIRROR, REDIS_SUITE)                       # redis 官方仓库
    index.add(DEBIAN_MIRROR, DEBIAN_SUITE)                     # 运行库
    index.add(DEBIAN_SECURITY, DEBIAN_SUITE + "-security", required=False)
    index.add(DEBIAN_MIRROR, DEBIAN_SUITE + "-updates", required=False)

    if "redis-server" not in index.packages:
        raise SystemExit("redis 索引里没有 redis-server —— 仓库地址或 suite 变了？")

    if args.redis_version:
        versions = index.all_versions(REDIS_MIRROR, REDIS_SUITE, "redis-server")
        match = [v for v in versions if args.redis_version in v]
        if not match:
            raise SystemExit("索引里没有 redis-server %s。可选的最新几个：\n  %s" % (
                args.redis_version,
                "\n  ".join(sorted(versions, key=AptIndex._vkey)[-8:])))
        entry = versions[max(match, key=AptIndex._vkey)]
    else:
        entry = index.packages["redis-server"]

    # Debian 的 epoch（6:）只是包管理用的，对外显示要去掉；
    # 尾巴 -1rl1~bookworm1 是 redis 自己的打包序号，同样不该露给用户。
    upstream = re.sub(r"^\d+:", "", entry[1]).split("-")[0]
    log("准备打包 redis-server %s（deb 版本 %s，%s）" % (upstream, entry[1], args.arch))

    stage = os.path.join(args.cache, "stage-deb", "redis-server-%s-%s" % (entry[1], args.arch))
    if not staged(stage):
        index.fetch_entry(entry, stage)

    modules = assemble(stage, out_dir, upstream)
    resolved, auto = resolve_closure(out_dir, ARCH_ELF[args.arch], index, args.cache)
    for soname, pkg in sorted(auto.items()):
        log("::warning:: %s 是靠 Contents 索引查到的（来自 %s）——"
            " 把它加进 SONAME_TO_DEB 能省下一次 40MB 的索引下载" % (soname, pkg))
    verify(out_dir, args.arch, modules)

    total = sum(os.path.getsize(os.path.join(r, f))
                for r, _d, fs in os.walk(out_dir) for f in fs
                if not os.path.islink(os.path.join(r, f)))
    log("完成：%s（%.1f MB，%d 个运行库）" % (out_dir, total / 1e6, len(resolved)))


if __name__ == "__main__":
    main()
