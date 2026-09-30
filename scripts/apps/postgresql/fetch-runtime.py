#!/usr/bin/env python3
"""把 pgdg 官方 Debian 包组装成一个能在绿联原生沙箱里跑起来的自包含 PostgreSQL 目录树。

要解决的三个问题（都是沙箱特有的，装普通 Debian 上一个都不会遇到）：

1. 【/usr 不存在】绿联原生应用的沙箱根目录只有 8 项 —— dev etc lib proc run sys var volume1
   （声明 SYSTEM.EXEC_SYSTEM_COMMAND 后会多出 /bin /usr/bin 等，但那是【过滤过的白名单】，
   而且 /usr/lib、/usr/share 依然没有）。所以从 glibc 和 ld.so 开始，整条依赖闭包都得自带，
   运行时用显式 loader 启动：
       ld-linux-<arch>.so.N --library-path <我们的lib> postgres ...
   闭包用 elfdeps.py 真解析 DT_NEEDED 得到，不是照记忆列一张表 —— 缺一个 .so 的表现是
   启动即退、只留一行 "No such file or directory"，极难查，所以【解析不出闭包就直接构建失败】。

2. 【时区数据库的路径是硬编码的】Debian 编 PostgreSQL 时用了 --with-system-tzdata=/usr/share/zoneinfo，
   这个绝对路径被编进 postgres 和 initdb 两个二进制里，而沙箱里没有 /usr/share。
   没有它 postgres 连启动都做不到（InitializeGUCOptions 里就要 pg_tzset("GMT")）。
   我们把这个字符串就地改成【相对路径 ../zoneinfo】，再由 launcher 保证每个子进程的 cwd
   的上一级有 zoneinfo 目录。选相对路径是因为沙箱里【没有任何一个短到能塞进 19 字节
   又确定可写】的绝对路径，而 cwd 是我们完全能控制的东西。
   见 patch_tzdir() 里的详细说明。

3. 【exec 出去的子进程要能自己找到 loader】initdb 会用 popen 起 "postgres --single"，
   那是 initdb 自己算出来的路径、我们插不上手。所以 bin/ 下每个可执行文件都拆成两个：
       postgres      ← 一行 sh 包装脚本，负责用自带 loader 启动
       postgres.bin  ← 真正的 ELF
   包装脚本用 exec，pid 不变，launcher 照常拿它当子进程守护。

用法:
    python3 fetch-runtime.py --arch arm64 --out <rootfs_arm64/pgsql> [--pg-major 17]
"""

import argparse
import gzip
import hashlib
import io
import lzma
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import elfdeps  # noqa: E402

PGDG_MIRROR = "https://apt.postgresql.org/pub/repos/apt"
PGDG_SUITE = "bookworm-pgdg"
DEBIAN_MIRROR = "https://deb.debian.org/debian"
DEBIAN_SECURITY = "https://deb.debian.org/debian-security"
# UGOS Pro 实测是 Debian 12 (bookworm)，运行库就照这个版本取，和 pgdg 的 ~pgdg12 对齐。
DEBIAN_SUITE = "bookworm"

ARCH_ELF = {"amd64": "x86_64", "arm64": "aarch64"}
ARCH_LOADER = {"amd64": "ld-linux-x86-64.so.2", "arm64": "ld-linux-aarch64.so.1"}

# 保留哪些可执行文件。全量约 20MB，但 pgbench / pg_test_* / oid2name / ecpg 这些
# 在 NAS 场景下没人会用，去掉能省下不少。留下的是"跑起来 + 备份还原 + 出事能救"这三类。
KEEP_BIN = [
    # 服务端（postgresql-17）
    "postgres", "initdb", "pg_ctl", "pg_controldata", "pg_resetwal",
    "pg_checksums", "pg_archivecleanup", "pg_waldump",
    # 客户端（postgresql-client-17）
    "psql", "pg_dump", "pg_dumpall", "pg_restore", "pg_isready",
    "createdb", "dropdb", "createuser", "dropuser",
    "vacuumdb", "reindexdb", "clusterdb",
    "pg_basebackup", "pg_verifybackup",
]

# 不打包的扩展模块：
#   llvmjit.so   —— JIT。它自己只有 130KB，但会把 libLLVM（100MB+）拖进闭包。
#                   PostgreSQL 只在 jit=on 且这个模块存在时才 dlopen 它，
#                   删掉 = 自动退回非 JIT 执行，功能完全正常（我们也会在
#                   postgresql.conf 里显式写 jit = off，免得日志里报找不到）。
SKIP_PGLIB = ("llvmjit.so",)
SKIP_PGLIB_DIRS = ("bitcode",)   # JIT 用的 .bc，几十 MB，没有 llvmjit 就是纯废重量

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
    "libicui18n.so.72": "libicu72", "libicuuc.so.72": "libicu72",
    "libicudata.so.72": "libicu72",
    "libgssapi_krb5.so.2": "libgssapi-krb5-2", "libkrb5.so.3": "libkrb5-3",
    "libk5crypto.so.3": "libk5crypto3", "libkrb5support.so.0": "libkrb5support0",
    "libcom_err.so.2": "libcom-err2", "libkeyutils.so.1": "libkeyutils1",
    "libldap-2.5.so.0": "libldap-2.5-0", "liblber-2.5.so.0": "libldap-2.5-0",
    "libsasl2.so.2": "libsasl2-2",
    "libpam.so.0": "libpam0g",
    "libsystemd.so.0": "libsystemd0",
    "libcap.so.2": "libcap2",
    "libgcrypt.so.20": "libgcrypt20", "libgpg-error.so.0": "libgpg-error0",
    "liblzma.so.5": "liblzma5", "libzstd.so.1": "libzstd1", "liblz4.so.1": "liblz4-1",
    "libz.so.1": "zlib1g",
    "libxml2.so.2": "libxml2", "libxslt.so.1": "libxslt1.1",
    "libselinux.so.1": "libselinux1", "libpcre2-8.so.0": "libpcre2-8-0",
    "libuuid.so.1": "libuuid1",
    "libreadline.so.8": "libreadline8", "libtinfo.so.6": "libtinfo6",
    "libnss_wrapper.so": "libnss-wrapper",
    "libpq.so.5": "libpq5",
    # 下面这些是二级依赖（libsystemd / libgnutls / libldap 各自又要的东西），
    # 都是第一次构建时靠 Contents 索引兜底查出来的，补进来省掉那 40MB 的索引下载。
    "libaudit.so.1": "libaudit1", "libcap-ng.so.0": "libcap-ng0",
    "libffi.so.8": "libffi8", "libgmp.so.10": "libgmp10",
    "libgnutls.so.30": "libgnutls30", "libhogweed.so.6": "libhogweed6",
    "libidn2.so.0": "libidn2-0", "libnettle.so.8": "libnettle8",
    "libp11-kit.so.0": "libp11-kit0", "libtasn1.so.6": "libtasn1-6",
    "libunistring.so.2": "libunistring2",
}

# 这些包从 pgdg 仓库取（PostgreSQL 官方 Debian 仓库），不是从 Debian 主仓库。
#
# postgresql-{major}-pgvector：向量扩展 pgvector。加它是为了让这个库能直接给
#   Immich 当后端用（Immich 要求库里有 pgvector 或 VectorChord，否则启动即报
#   "No vector extension found"）。用 pgdg 的预编译 arm64 deb，和 postgres 本体
#   同源同 ABI，不用自己编。它解包出 usr/lib/postgresql/17/lib/vector.so 和
#   usr/share/postgresql/17/extension/vector*，会被 assemble() 的通配 .so 规则和
#   整份 share 拷贝自动带上；它的 DT_NEEDED 只有 libc/libm，闭包本来就有。
#   —— VectorChord(vchord) 不在 pgdg（在 tensorchord 自家源），而 pgvector 0.8.x
#      Immich 已完整支持（DB_VECTOR_EXTENSION=vector），够用，所以只加这一个。
PGDG_PACKAGES = ["postgresql-{major}", "postgresql-client-{major}", "postgresql-{major}-pgvector"]
# libpq5 也在 pgdg 里（版本比 Debian 的新），dblink / postgres_fdw / psql 都要它。
PGDG_EXTRA = ["libpq5"]


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

    必须校验：HTTP 连接中途断掉时 copyfileobj 只是少写几个字节，
    不抛任何异常 —— 于是缓存里留下一个看起来正常的半截文件，
    然后在很后面的某一步炸出一个完全指错方向的错。
    实测就这么栽过一次：libstdc++6 的 deb 少了一截，表现是
    "Debian 包 libstdc++6 里没有 soname 为 libstdc++.so.6 的库"。
    Debian 的 Packages 索引本来就带 Size 和 SHA256，白给的东西没理由不用。
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
            # 4xx 不重试，直接往上抛：调用方可能正靠它做"先试 .gz 再试 .xz"这类探测。
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

    为什么不直接用 urllib：实测 deb.debian.org 上的某些文件用 urllib 下会【静默截断】
    （libstdc++6 那个包每次都在 45~46 万字节处断掉，实际是 556608 字节），
    而同一时刻 curl 拉同一个 URL 每次都完整。curl 自带重试、超时分级和断点续传，
    这种事它处理得比我们手写的循环好。urllib 留作没有 curl 时的兜底。
    """
    if shutil.which("curl"):
        # 【直接读 HTTP 状态码，不靠 curl 的退出码判断 404】：
        # 实测 deb.debian.org 的 404 让 curl 退出码是 56（"The requested URL returned
        # error: 404"）而不是文档里说的 22，靠退出码分类会把"没这个文件"误判成"网络故障"，
        # 于是 download_index 的 ".gz 不行就试 .xz" 探测就废了。
        # 也不加 --retry-all-errors：那会连 404 都重试三遍，纯属浪费。
        r = subprocess.run(
            ["curl", "-sSL", "--retry", "3",
             "--connect-timeout", "30", "--max-time", "900",
             "-A", "postgresql-ugreen-app/build",
             "-w", "%{http_code}", "-o", tmp, url],
            capture_output=True)
        code = r.stdout.decode("ascii", "replace").strip()[-3:]
        if code.startswith("4"):
            raise NotFound("%s → HTTP %s" % (url, code))
        if r.returncode != 0 or not code.startswith("2"):
            raise RuntimeError("curl 退出码 %d，HTTP %s：%s" % (
                r.returncode, code or "?", r.stderr.decode("utf-8", "replace").strip()))
        return
    req = urllib.request.Request(url, headers={"User-Agent": "postgresql-ugreen-app/build"})
    try:
        with urllib.request.urlopen(req, timeout=180) as resp, open(tmp, "wb") as f:
            shutil.copyfileobj(resp, f)
    except urllib.error.HTTPError as exc:
        raise NotFound(url) from exc


def download_index(url_base, dest_base):
    """下载 Debian 索引并解压成文本。压缩格式各仓库不一致（security 只有 .xz），逐个试。"""
    last_err = None
    for suffix, opener in ((".gz", gzip.open), (".xz", lzma.open)):
        try:
            path = download(url_base + suffix, dest_base + suffix)
        except NotFound as e:
            last_err = e
            continue
        return opener(path, "rt", encoding="utf-8", errors="replace")
    raise last_err or RuntimeError("索引下载失败：%s" % url_base)


def ar_members(path):
    """解析 ar 归档，产出 (成员名, 数据)。

    自己解析而不是调系统 ar，图的是不依赖平台上有什么、以及报错能指对地方：
    macOS 的 BSD ar 遇到问题只会说一句 "Inappropriate file type or format"，
    看不出是文件坏了还是格式不认识（实测被它拒绝的那个 libstdc++6 的 deb，
    后来查明是【下载被截断】，见 download() —— 当时却让人以为是 ar 的兼容性问题，
    白绕了一圈）。ar 格式本身简单到不值得为它引入平台依赖：
    8 字节魔数 + 每个成员 60 字节定长头 + 数据（按偶数字节对齐）。
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
            off = int(name[1:])           # GNU 长名：指向长名表里的偏移
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
    if deb_path.endswith(".zst") or data[:4] == b"\x28\xb5\x2f\xfd":
        # tarfile 不认 zstd，交给外部 zstd。Debian 12 与 pgdg 目前都用 xz，这只是兜底。
        data = subprocess.run(["zstd", "-dc"], check=True, input=data,
                              stdout=subprocess.PIPE).stdout
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:   # 自动识别 xz/gz/bz2
        tf.extractall(dest_dir)


class AptIndex:
    """一个 apt 仓库的 Packages 索引（可叠加多个仓库，后加载的覆盖先加载的）。"""

    def __init__(self, arch, cache):
        self.arch = arch
        self.cache = cache
        self.packages = {}      # name → (url, version)
        self._contents = None

    @staticmethod
    def _vkey(version):
        """dpkg 风格的版本比较键：按"数字段 / 非数字段"交替切开，数字段按数值比。

        必须这么比而不能直接比字符串：pgdg 索引里【同一个包有多个版本条目】，
        字符串序会把 17.9 排在 17.10 后面（'9' > '1'），于是"最新版"会选错。
        这个坑很隐蔽 —— 打出来的包能用，只是悄悄比上游落后一个小版本。
        """
        parts = re.findall(r"(\d+)|(\D+)", version or "")
        return [(0, int(d)) if d else (1, s) for d, s in parts]

    def add(self, mirror, suite, required=True, component="main"):
        url_base = "%s/dists/%s/%s/binary-%s/Packages" % (mirror, suite, component, self.arch)
        dest_base = os.path.join(self.cache, "idx", "%s_%s_%s_Packages" % (
            suite.replace("/", "_"), component, self.arch))
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
        url, version, size, sha = self.packages[pkg]
        deb = download(url, os.path.join(self.cache, "deb", os.path.basename(url)),
                       size=size, sha256=sha)
        extract_deb(deb, stage)
        with open(os.path.join(stage, ".extracted"), "w") as f:
            f.write(version + "\n")
        return version


def staged(stage):
    """这个 stage 目录是不是【解包成功】过。

    不能只判断目录在不在：解包失败时目录已经建出来了，只是空的，
    下次跑就会被当成有效缓存直接跳过，然后在很靠后的地方报一个
    完全指错方向的错（"soname→包的映射错了"）。这个坑真踩过。
    """
    return os.path.exists(os.path.join(stage, ".extracted"))


def pick_pg_version(index, major):
    """在 pgdg 索引里挑出 postgresql-<major> 的可用版本（索引里每个包只有最新一版）。"""
    name = "postgresql-%s" % major
    if name not in index.packages:
        raise SystemExit("pgdg 索引里没有 %s —— 这个大版本还没发布或已下架？" % name)
    return index.packages[name][1]


def install_soname(src_roots, lib_dir, soname, elf_arch):
    """从 src_roots 里找出 SONAME 等于 soname 的那一个共享库，装进 lib_dir。

    【只装点名要的那一个】，不是把整棵树里的 .so 一扫而空 —— 后者的代价是
    libc6 的 deb 里几百个 gconv 编码模块会全被扫进来。按需安装还有个好处：
    装进来的每一个库都是闭包真正要求的，没有"以防万一"。

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


def resolve_closure(out_dir, elf_arch, index, cache, pg_stage, major, max_rounds=10):
    """反复解析 DT_NEEDED 闭包，缺什么补什么，直到不再缺。

    补的来源有先后：先在 pgdg 自己的解包树里找（版本要和 postgres 配套），
    找不到才去 Debian 仓库拉。
    """
    lib_dir = os.path.join(out_dir, "lib", "runtime")
    roots = [os.path.join(out_dir, "lib", "postgresql", major, "bin"),
             os.path.join(out_dir, "lib", "postgresql", major, "lib")]
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
            if install_soname([pg_stage], lib_dir, soname, elf_arch):
                log("  %s ← pgdg 自带" % soname)
                progressed = True
                continue
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


def assemble(stage, out_dir, arch, major):
    """从解包出来的 Debian 树里挑出要的东西，组装成最终布局。

    布局刻意【保留 Debian 的相对结构】（lib/postgresql/<major>/bin 与
    share/postgresql/<major> 成对出现），因为 PostgreSQL 的 make_relative_path()
    是拿编译期的 PGBINDIR 与 PGSHAREDIR 求公共前缀、再把剩下的尾巴套到
    my_exec_path 上的：
        PGBINDIR   = /usr/lib/postgresql/17/bin
        PGSHAREDIR = /usr/share/postgresql/17
    公共前缀是 "/usr/"，于是它要求"可执行文件所在目录以 lib/postgresql/17/bin 结尾"，
    满足了才会把 share 解析成 <我们的根>/share/postgresql/17。
    【改扁这个布局 = PostgreSQL 找不到 postgres.bki，initdb 当场失败。】
    同理 PKGLIBDIR=/usr/lib/postgresql/17/lib 解析成 <根>/lib/postgresql/17/lib。
    """
    elf_arch = ARCH_ELF[arch]
    shutil.rmtree(out_dir, ignore_errors=True)
    bin_dir = os.path.join(out_dir, "lib", "postgresql", major, "bin")
    pglib_dir = os.path.join(out_dir, "lib", "postgresql", major, "lib")
    share_dir = os.path.join(out_dir, "share", "postgresql", major)
    for d in (bin_dir, pglib_dir, share_dir, os.path.join(out_dir, "lib", "runtime")):
        os.makedirs(d, exist_ok=True)

    src_bin = os.path.join(stage, "usr/lib/postgresql", major, "bin")
    for name in KEEP_BIN:
        src = os.path.join(src_bin, name)
        if not os.path.exists(src):
            raise SystemExit("没找到可执行文件 %s（上游包内容变了？）" % name)
        dst = os.path.join(bin_dir, name)
        shutil.copy2(src, dst)
        os.chmod(dst, 0o755)

    src_pglib = os.path.join(stage, "usr/lib/postgresql", major, "lib")
    kept = 0
    for name in sorted(os.listdir(src_pglib)):
        src = os.path.join(src_pglib, name)
        if name in SKIP_PGLIB or name in SKIP_PGLIB_DIRS:
            continue
        if os.path.isdir(src) or os.path.islink(src) or not name.endswith(".so"):
            continue
        shutil.copy2(src, os.path.join(pglib_dir, name))
        os.chmod(os.path.join(pglib_dir, name), 0o755)
        kept += 1
    log("扩展模块：保留 %d 个 .so（已排除 %s 与 bitcode/）" % (kept, ", ".join(SKIP_PGLIB)))

    src_share = os.path.join(stage, "usr/share/postgresql", major)
    for name in sorted(os.listdir(src_share)):
        src = os.path.join(src_share, name)
        dst = os.path.join(share_dir, name)
        # symlinks=False：一律解引用成真实文件。upk 打包对符号链接的处理没验证过，
        # 赌错了的表现是运行时莫名其妙缺文件，不值得冒这个险。
        if os.path.isdir(src):
            shutil.copytree(src, dst, symlinks=False)
        else:
            shutil.copy2(src, dst)
    if not os.path.exists(os.path.join(share_dir, "postgres.bki")):
        raise SystemExit("share 里没有 postgres.bki —— initdb 一定跑不起来")
    return elf_arch


def install_tzdata(tz_stage, out_dir):
    """把 tzdata 的 zoneinfo 装到 <out>/zoneinfo。

    只要真正的时区文件：posix/ 和 right/ 是同一份数据的两个副本
    （right/ 是含闰秒的版本，PostgreSQL 用不到），去掉能省 2/3 体积。
    符号链接（Asia/Chongqing → Shanghai 这类）一律解引用成真实文件，
    不去赌 upk 打包对链接的处理 —— 赌错的表现是某些时区莫名其妙不可用。
    """
    src = os.path.join(tz_stage, "usr/share/zoneinfo")
    if not os.path.isdir(src):
        raise SystemExit("tzdata 包里没有 usr/share/zoneinfo")
    dst = os.path.join(out_dir, "zoneinfo")
    shutil.rmtree(dst, ignore_errors=True)
    shutil.copytree(src, dst, symlinks=False,
                    ignore=shutil.ignore_patterns("posix", "right"))
    n = sum(len(fs) for _r, _d, fs in os.walk(dst))
    if not os.path.exists(os.path.join(dst, "UTC")):
        raise SystemExit("zoneinfo 里没有 UTC —— PostgreSQL 启动时一定会失败")
    log("时区数据：%d 个文件" % n)


TZ_OLD = b"/usr/share/zoneinfo\x00"
TZ_NEW = b"../zoneinfo\x00"


def patch_tzdir(out_dir, major):
    """把二进制里硬编码的 /usr/share/zoneinfo 改成相对路径 ../zoneinfo。

    为什么必须改：Debian 用 --with-system-tzdata 编译，pg_TZDIR() 直接返回这个
    编译期常量（不看任何环境变量，PGSHAREDIR 那套相对路径推导对它不生效）。
    沙箱里没有 /usr/share → postgres 在 InitializeGUCOptions() 里 pg_tzset("GMT")
    就会失败，【连 postgresql.conf 都读不到】。

    为什么用相对路径而不是换一个绝对路径：替换串不能比原串长（19 字节），
    而沙箱里【没有任何一个这么短又确定可写】的绝对路径可用
    （/volume1/@appdata/ 光前缀就 18 字节；/tmp、/run 是否可写不确定，
     而且要赌固件行为）。cwd 则是我们 100% 能控制的东西：
       * postgres —— launcher 把 cwd 设成 PGDATA，且 postmaster 自己也会
         ChangeToDataDir() 到同一个地方，前后一致；
       * initdb —— launcher 把 cwd 设成 <数据目录>/init-cwd；
       * initdb popen 出来的 "postgres --single" 继承 initdb 的 cwd。
    三者的 ../zoneinfo 都落到 <数据目录>/zoneinfo，由 launcher 建成指向本目录的软链。

    安全性：只有 postgres 和 initdb 两个文件含这个串，且每个文件里【只出现一次】
    （下面会断言）。替换后补 NUL 保持长度不变，不动任何偏移。
    """
    bin_dir = os.path.join(out_dir, "lib", "postgresql", major, "bin")
    patched = []
    for name in sorted(os.listdir(bin_dir)):
        path = os.path.join(bin_dir, name)
        with open(path, "rb") as f:
            data = f.read()
        n = data.count(TZ_OLD)
        if n == 0:
            continue
        if n != 1:
            raise SystemExit(
                "%s 里 %s 出现了 %d 次，预期 1 次 —— 上游改了构建方式，"
                "请人工确认后再改这里的替换逻辑" % (name, TZ_OLD, n))
        # 必须是一个独立字符串的开头（前一个字节是 NUL），不能是某条更长路径的尾巴 ——
        # 否则替换会把别人的字符串拦腰截断。
        off = data.find(TZ_OLD)
        if off == 0 or data[off - 1] != 0:
            raise SystemExit(
                "%s 里 %s 的前一个字节不是 NUL（在 0x%x），它可能是另一个更长字符串的一部分，"
                "需要人工确认" % (name, TZ_OLD, off))
        # 注意【不能】用 b"zoneinfo" 做全局残留检查：pg_config 的编译参数原样存在
        # 二进制里，其中就有 '--with-system-tzdata=/usr/share/zoneinfo'（后面跟的是
        # 单引号不是 NUL）。那是另一个字符串，不该动也动不着。
        # 同长度替换：新串 + 补 NUL。绝不改变任何字节偏移。
        new = TZ_NEW + b"\x00" * (len(TZ_OLD) - len(TZ_NEW))
        assert len(new) == len(TZ_OLD)
        data = data[:off] + new + data[off + len(TZ_OLD):]
        with open(path, "wb") as f:
            f.write(data)
        patched.append(name)
    if sorted(patched) != ["initdb", "postgres"]:
        raise SystemExit(
            "预期只有 initdb 和 postgres 需要打 tzdata 补丁，实际是 %s —— "
            "上游变了，先搞清楚再继续" % (patched or "一个都没有"))
    log("tzdata 路径补丁：%s（/usr/share/zoneinfo → ../zoneinfo）" % ", ".join(patched))


WRAPPER = """#!/bin/sh
# 由 fetch-runtime.py 生成，勿手改。
# 用自带的 ld.so + 自带的运行库启动真正的二进制（沙箱里没有 /usr/lib，
# 系统 loader 的路径也不由我们决定）。用 exec 保证 pid 不变，
# 这样 launcher 的守护逻辑和 initdb 的 popen 都当它是同一个进程。
case $0 in
  */*) d=${0%%/*} ;;
  *) echo "$0: 必须用路径调用（沙箱里没有可靠的 PATH）" >&2; exit 127 ;;
esac
r=$d/../../../runtime
exec "$r/%(loader)s" --library-path "$r" "$d/${0##*/}.bin" "$@"
"""


def make_wrappers(out_dir, arch, major):
    """把 bin/X 改名成 bin/X.bin，并生成同名的 sh 包装脚本。

    为什么不能只靠 launcher 显式调 ld.so：initdb 会 popen 一个它【自己算出来的】
    "<和 initdb 同目录>/postgres --single ..."，那条命令行我们插不上手。
    包装脚本就是为这种"程序自己 exec 程序"的场合准备的。
    """
    bin_dir = os.path.join(out_dir, "lib", "postgresql", major, "bin")
    loader = ARCH_LOADER[arch]
    for name in sorted(os.listdir(bin_dir)):
        if name.endswith(".bin"):
            continue
        real = os.path.join(bin_dir, name)
        os.rename(real, real + ".bin")
        with open(real, "w") as f:
            f.write(WRAPPER % {"loader": loader})
        os.chmod(real, 0o755)
    log("已生成 %d 个 loader 包装脚本" % len(KEEP_BIN))


def verify(out_dir, arch, major):
    """构建的最后一道闸。这里每一条都是"错了就在真机上启动即退、且日志没法看"的那类问题。"""
    elf_arch = ARCH_ELF[arch]
    loader = ARCH_LOADER[arch]
    bin_dir = os.path.join(out_dir, "lib", "postgresql", major, "bin")
    lib_dir = os.path.join(out_dir, "lib", "runtime")
    problems = []

    if not os.path.exists(os.path.join(lib_dir, loader)):
        problems.append("缺少动态加载器 lib/runtime/%s —— 没有它整个包在沙箱里跑不起来" % loader)

    # 每个包装脚本都要有对应的 .bin，且 .bin 架构正确
    for name in sorted(os.listdir(bin_dir)):
        if name.endswith(".bin"):
            continue
        real = os.path.join(bin_dir, name + ".bin")
        if not os.path.exists(real):
            problems.append("包装脚本 %s 没有对应的 %s.bin" % (name, name))
            continue
        if not (os.stat(os.path.join(bin_dir, name)).st_mode & stat.S_IXUSR):
            problems.append("包装脚本 %s 没有可执行位" % name)

    # 依赖闭包不能有缺口
    resolved, missing = elfdeps.closure(
        [lib_dir],
        [bin_dir, os.path.join(out_dir, "lib", "postgresql", major, "lib")])
    if missing:
        problems.append("依赖闭包仍有缺口：%s" % ", ".join(sorted(missing)))

    # 所有 ELF 架构必须一致（防止某天预编译缺架构、静默混进宿主架构的文件）
    for path, elf in elfdeps.iter_elf([out_dir]):
        if elf.arch != elf_arch:
            problems.append("架构不符：%s 是 %s" % (os.path.relpath(path, out_dir), elf.arch))

    # tzdata 补丁真的打上了
    for name in ("postgres", "initdb"):
        with open(os.path.join(bin_dir, name + ".bin"), "rb") as f:
            data = f.read()
        if TZ_OLD in data:
            problems.append("%s 里仍有 /usr/share/zoneinfo，补丁没打上" % name)
        if TZ_NEW not in data:
            problems.append("%s 里没有 ../zoneinfo，补丁打歪了" % name)

    # 关键数据文件
    share = os.path.join(out_dir, "share", "postgresql", major)
    for rel in ("postgres.bki", "postgresql.conf.sample", "pg_hba.conf.sample"):
        if not os.path.exists(os.path.join(share, rel)):
            problems.append("share 里缺 %s" % rel)
    n_ext = len(os.listdir(os.path.join(share, "extension")))
    if n_ext < 100:
        problems.append("contrib 扩展只有 %d 个文件，明显不对" % n_ext)
    if not os.path.exists(os.path.join(out_dir, "zoneinfo", "Asia", "Shanghai")):
        problems.append("zoneinfo 里没有 Asia/Shanghai")
    if not os.path.exists(os.path.join(lib_dir, "libnss_wrapper.so")):
        problems.append("缺 libnss_wrapper.so —— 沙箱里没有 /etc/passwd，"
                        "initdb 的 getpwuid 会失败")

    if problems:
        for p in problems:
            print("::error:: %s" % p, file=sys.stderr)
        raise SystemExit("校验未通过")
    log("校验通过：%s，闭包 %d 个库，扩展 %d 个文件，loader=%s"
        % (elf_arch, len(resolved), n_ext, loader))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arch", required=True, choices=["amd64", "arm64"])
    ap.add_argument("--pg-major", default="17")
    ap.add_argument("--out", required=True, help="输出目录，如 rootfs_arm64/pgsql")
    ap.add_argument("--cache", default=None)
    ap.add_argument("--print-version", action="store_true",
                    help="只打印 pgdg 上的当前版本号，不构建")
    args = ap.parse_args()

    cache = args.cache or os.path.join(
        os.path.expanduser("~"), ".cache", "postgresql-ugreen-app")
    major = args.pg_major

    index = AptIndex(args.arch, cache)
    index.add(PGDG_MIRROR, PGDG_SUITE, required=True)
    version = pick_pg_version(index, major)
    if args.print_version:
        print(version)
        return
    log("PostgreSQL %s（%s，%s）" % (version, PGDG_SUITE, args.arch))

    # pgdg 索引先加载，Debian 主仓库/security 后加载 —— 但 packages 字典是
    # 后写覆盖先写，所以要保证 pgdg 独有的包名不被覆盖。这里没有重名风险
    # （pgdg 的 libpq5 版本更高，我们想要的正是 pgdg 那版），所以反过来加载：
    # 先记住 pgdg 的条目，加载完 Debian 之后再放回去。
    pgdg_only = {k: v for k, v in index.packages.items()}
    index.add(DEBIAN_MIRROR, DEBIAN_SUITE, required=True)
    index.add(DEBIAN_SECURITY, DEBIAN_SUITE + "-security", required=False)
    for name in PGDG_EXTRA + [p.format(major=major) for p in PGDG_PACKAGES]:
        if name in pgdg_only:
            index.packages[name] = pgdg_only[name]

    stage = os.path.join(cache, "stage-pg-%s-%s" % (version, args.arch))
    shutil.rmtree(stage, ignore_errors=True)
    for tmpl in PGDG_PACKAGES:
        index.fetch(tmpl.format(major=major), stage)
    log("pgdg 包解包完成")

    tz_stage = os.path.join(cache, "stage-tzdata")
    if not staged(tz_stage):
        index.fetch("tzdata", tz_stage)

    elf_arch = assemble(stage, args.out, args.arch, major)
    install_tzdata(tz_stage, args.out)
    patch_tzdir(args.out, major)

    # libnss_wrapper 要先装进来再解闭包（它自己也有 DT_NEEDED）。
    # 沙箱的 /etc 只有 localtime resolv.conf ssl timezone 四项，【没有 passwd】，
    # 而 initdb 和 "postgres --single" 都会 getpwuid(geteuid()) 去问自己叫什么名字，
    # 查不到就直接退出（"could not look up effective user ID"）。
    # nss_wrapper 用 LD_PRELOAD 把这个查询接管掉，由 launcher 在运行时生成一份
    # 只有一行的 passwd 文件。
    nssw_stage = os.path.join(cache, "stage-deb", "libnss-wrapper-%s" % args.arch)
    if not staged(nssw_stage):
        index.fetch("libnss-wrapper", nssw_stage)
    if not install_soname([nssw_stage], os.path.join(args.out, "lib", "runtime"),
                          "libnss_wrapper.so", elf_arch):
        raise SystemExit("libnss-wrapper 包里没找到 libnss_wrapper.so")

    _resolved, auto_found = resolve_closure(args.out, elf_arch, index, cache, stage, major)
    make_wrappers(args.out, args.arch, major)
    verify(args.out, args.arch, major)

    if auto_found:
        log("提示：以下 soname 是靠 Contents 索引兜底找到的，建议补进 SONAME_TO_DEB：")
        for so, pkg in sorted(auto_found.items()):
            log('    "%s": "%s",' % (so, pkg))

    total = sum(os.path.getsize(os.path.join(r, f))
                for r, _d, fs in os.walk(args.out) for f in fs)
    log("完成：%s（%.1f MB）" % (args.out, total / 1024.0 / 1024.0))
    with open(os.path.join(args.out, "VERSION"), "w") as f:
        f.write(version + "\n")


if __name__ == "__main__":
    main()
