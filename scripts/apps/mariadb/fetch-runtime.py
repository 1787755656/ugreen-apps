#!/usr/bin/env python3
"""把 MariaDB 的 Debian 包 + 全部运行库组装成一个自包含目录树。

为什么要这么干（这是整个原生包能不能跑起来的关键）：
  绿联原生应用的沙箱根目录只有 8 项 —— dev etc lib proc run sys var volume1。
  【/usr 根本不存在】，/lib 里有什么也不由我们决定、还会随固件升级变。
  MariaDB 是动态链接的 C++ 程序，依赖 glibc / OpenSSL / libstdc++ / libsystemd…
  所以唯一稳妥的做法是：把【包括 glibc 和 ld.so 在内】的整条依赖闭包一起打进包，
  运行时用显式 loader 启动：
      ld-linux-<arch>.so.N --library-path <我们的lib> mariadbd ...
  这样既不依赖沙箱里的 /lib，也不需要 patchelf。

闭包用 elfdeps.py 真正解析 DT_NEEDED 得到，不是照着记忆写一张表 ——
缺一个 .so 的表现是启动即退、日志里只有一行 "No such file or directory"，
非常难查，所以这里【解析不出闭包就直接构建失败】。

用法:
    python3 fetch-runtime.py --arch arm64 --mariadb-version 11.4.12 \\
        --out <rootfs_arm64目录> [--cache <下载缓存目录>]
"""

import argparse
import gzip
import hashlib
import io
import lzma
import os
import shutil
import subprocess
import sys
import tarfile
import typing
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import elfdeps  # noqa: E402

MARIADB_MIRROR = "https://archive.mariadb.org"
DEBIAN_MIRROR = "https://deb.debian.org/debian"
DEBIAN_SECURITY = "https://deb.debian.org/debian-security"
# UGOS Pro 实测是 Debian 12 (bookworm)，运行库就照这个版本取，
# 和 MariaDB 官方 deb 的 ~deb12 后缀对齐。
DEBIAN_SUITE = "bookworm"

# MariaDB 官方 deb 仓库里要取的包。deb12 后缀的是按 Debian 12 构建的。
MARIADB_PACKAGES = [
    ("mariadb-server", True),        # mariadbd 的配套脚本/插件/share
    ("mariadb-server-core", True),   # mariadbd 本体
    ("mariadb-client-core", True),   # mariadb 客户端
    ("mariadb-client", True),        # mariadb-dump 等
    ("mariadb-common", False),       # arch=all
]

# 只保留真正用得上的可执行文件。usr/bin 全量有 78MB，绝大部分是
# 我们跑不到也用不上的工具（aria_* / myisam* / wsrep_* / innochecksum…）。
KEEP_BIN = [
    "mariadb",           # 交互式客户端
    "mariadb-dump",      # 备份，数据库应用必备
    "mariadb-check",     # mariadb-upgrade 会调它
    "mariadb-upgrade",   # 小版本升级后要跑
]

# 不打包的插件：
#   auth_pam*  —— 沙箱里没有 /etc/pam.d，PAM 认证不可能成立，
#                 还会把 libpam.so.0 拖进闭包，纯粹是负担。
SKIP_PLUGINS = ("auth_pam.so", "auth_pam_v1.so", "auth_pam_tool_dir")

# soname → Debian 包名。只是"快路径"：查不到的会自动去 Contents 索引里找，
# 所以这张表过时了不会导致构建出错误的包，只会慢一点并提示你补上。
SONAME_TO_DEB = {
    "libc.so.6": "libc6", "libm.so.6": "libc6", "libdl.so.2": "libc6",
    "libpthread.so.0": "libc6", "librt.so.1": "libc6", "libresolv.so.2": "libc6",
    "ld-linux-aarch64.so.1": "libc6", "ld-linux-x86-64.so.2": "libc6",
    "libcrypt.so.1": "libcrypt1",
    "libgcc_s.so.1": "libgcc-s1",
    "libstdc++.so.6": "libstdc++6",
    "libssl.so.3": "libssl3", "libcrypto.so.3": "libssl3",
    "libaio.so.1": "libaio1",
    "liburing.so.2": "liburing2",
    "libpcre2-8.so.0": "libpcre2-8-0",
    "libz.so.1": "zlib1g",
    "libsystemd.so.0": "libsystemd0",
    "libcap.so.2": "libcap2",
    "libgcrypt.so.20": "libgcrypt20",
    "libgpg-error.so.0": "libgpg-error0",
    "liblzma.so.5": "liblzma5",
    "libzstd.so.1": "libzstd1",
    "liblz4.so.1": "liblz4-1",
    "libncurses.so.6": "libncurses6",
    "libtinfo.so.6": "libtinfo6",
    "libedit.so.2": "libedit2",
    "libbsd.so.0": "libbsd0",
    "libmd.so.0": "libmd0",
    "libnuma.so.1": "libnuma1",
}

ARCH_ELF = {"amd64": "x86_64", "arm64": "aarch64"}
ARCH_LOADER = {"amd64": "ld-linux-x86-64.so.2", "arm64": "ld-linux-aarch64.so.1"}


def log(msg):
    print("[fetch-runtime] %s" % msg, flush=True)


def download(url, dest, expected_sha256=None, expected_size=None, retries=3):
    """下载到 dest，并【校验完整性】。

    为什么非校验不可（踩过）：urllib 的响应流在连接中途断开时不会抛异常，
    shutil.copyfileobj 只会安静地少写一些字节，于是半截文件被当成完整的落盘。
    表现是几步之后才炸 —— 实测是 macOS 的 ar 报一句
    "Inappropriate file type or format"，完全指不到"文件下载不全"这个真因上。

    校验两道：先比 Content-Length（任何服务器都给），
    有 SHA256 的（Debian 的 Packages 索引里带）再比一次哈希。
    """
    if os.path.exists(dest) and os.path.getsize(dest) > 0:
        if _verify_file(dest, expected_sha256, expected_size):
            return dest
        log("缓存文件 %s 校验不过，重新下载" % os.path.basename(dest))
        os.remove(dest)

    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".part"
    last_err = None
    for attempt in range(1, retries + 1):
        log("下载 %s%s" % (url, "" if attempt == 1 else "（第 %d 次尝试）" % attempt))
        try:
            req = urllib.request.Request(
                url, headers={"User-Agent": "mariadb-ugreen-app/build"})
            with urllib.request.urlopen(req, timeout=300) as r:
                declared = r.headers.get("Content-Length")
                with open(tmp, "wb") as f:
                    shutil.copyfileobj(r, f)
            got = os.path.getsize(tmp)
            if declared is not None and got != int(declared):
                raise IOError("只收到 %d 字节，服务器声明 %s 字节（连接被截断）"
                              % (got, declared))
            if not _verify_file(tmp, expected_sha256, expected_size):
                raise IOError("校验和不匹配")
        except urllib.error.HTTPError:
            raise  # 404 之类不该重试，交给调用方（download_index 靠它试下一个后缀）
        except Exception as exc:
            last_err = exc
            log("::warning:: 下载失败：%s" % exc)
            if os.path.exists(tmp):
                os.remove(tmp)
            continue
        os.rename(tmp, dest)
        return dest
    raise RuntimeError("下载 %s 失败（重试 %d 次）：%s" % (url, retries, last_err))


def _verify_file(path, expected_sha256, expected_size):
    if expected_size is not None and os.path.getsize(path) != expected_size:
        return False
    if expected_sha256:
        h = hashlib.sha256()
        with open(path, "rb") as f:
            for block in iter(lambda: f.read(1 << 20), b""):
                h.update(block)
        if h.hexdigest() != expected_sha256.lower():
            return False
    return True


def download_index(url_base, dest_base):
    """下载 Debian 索引文件并解压成文本。

    压缩格式各仓库不一致：主仓库同时提供 .gz 和 .xz，
    而 security 仓库【只有 .xz】（实测 .gz 直接 404）。所以逐个试。
    """
    last_err = None
    for suffix, opener in ((".gz", gzip.open), (".xz", lzma.open)):
        try:
            path = download(url_base + suffix, dest_base + suffix)
        except urllib.error.HTTPError as e:
            last_err = e
            continue
        return opener(path, "rt", encoding="utf-8", errors="replace")
    raise last_err or RuntimeError("索引下载失败：%s" % url_base)


def read_ar_members(path):
    """读 ar 归档，产出 (成员名, 字节内容)。

    为什么自己解而不是调 `ar`：macOS 自带的 ar 对某些 Debian 包会报
    "Inappropriate file type or format" 直接失败（实测 libssl3 的 amd64 包必现，
    而同一个文件 `ar t` 却能正常列出内容）。开发机没有 dpkg-deb，
    与其赌系统 ar 的脾气，不如自己解 —— 格式一共就这么几行，
    顺带让 macOS 和 Linux（CI）上的行为完全一致。

    ar 的格式：8 字节魔数，之后每个成员一个 60 字节头 + 数据（按偶数字节对齐）。
    头里的字段都是右侧补空格的 ASCII。
    """
    with open(path, "rb") as f:
        if f.read(8) != b"!<arch>\n":
            raise RuntimeError("%s 不是 ar 归档" % path)
        long_names = b""
        while True:
            header = f.read(60)
            if len(header) < 60:
                return
            name = header[0:16].decode("ascii", "replace").strip()
            size_field = header[48:58].decode("ascii", "replace").strip()
            if header[58:60] != b"`\n":
                raise RuntimeError("%s 的成员头损坏（缺少结束标记）" % path)
            size = int(size_field)
            data = f.read(size)
            if size % 2:
                f.read(1)  # 成员之间按偶数字节对齐

            # GNU 的长文件名表：整张表存在名为 "//" 的成员里，
            # 其它成员用 "/<偏移>" 引用它。Debian 的包用不到，但解了不亏。
            if name == "//":
                long_names = data
                continue
            if name.startswith("/") and name[1:].isdigit():
                off = int(name[1:])
                end = long_names.find(b"/\n", off)
                if end < 0:
                    end = long_names.find(b"\n", off)
                name = long_names[off:end].decode("ascii", "replace")
            # GNU 在短名字后面补一个 "/" 作结束符
            name = name.rstrip("/")
            if name == "/" or name == "":
                continue
            yield name, data


def extract_deb(deb_path, dest_dir):
    """解开 .deb（ar 归档里的 data.tar.{xz,zst,gz}）到 dest_dir。"""
    os.makedirs(dest_dir, exist_ok=True)
    data = None
    data_name = None
    for name, blob in read_ar_members(deb_path):
        if name.startswith("data.tar"):
            data, data_name = blob, name
            break
    if data is None:
        raise RuntimeError("%s 里没有 data.tar.*" % deb_path)

    if data_name.endswith(".zst"):
        # tarfile 不认 zstd，交给外部 zstd。MariaDB 与 Debian 12 目前都用 xz，
        # 这条只是给将来上游换压缩格式留的兜底。
        data = subprocess.run(["zstd", "-dc"], input=data, check=True,
                              stdout=subprocess.PIPE).stdout
    # tarfile 自动识别 xz/gz/bz2；未压缩的 .tar 也认
    with tarfile.open(fileobj=io.BytesIO(data)) as tf:
        # 保留符号链接原样解开；后面统一按 SONAME 落地，不依赖这些链接。
        tf.extractall(dest_dir)


def fetch_mariadb(version, arch, cache, stage):
    base = "%s/mariadb-%s/repo/debian/pool/main/m/mariadb" % (MARIADB_MIRROR, version)
    for pkg, arch_specific in MARIADB_PACKAGES:
        suffix = arch if arch_specific else "all"
        fname = "%s_%s+maria~deb12_%s.deb" % (pkg, version, suffix)
        url = "%s/%s" % (base, urllib.parse.quote(fname))
        deb = download(url, os.path.join(cache, "mariadb", fname))
        extract_deb(deb, stage)
    log("MariaDB %s (%s) 解包完成" % (version, arch))


class DebPackage(typing.NamedTuple):
    url: str
    version: str
    sha256: typing.Optional[str]
    size: typing.Optional[int]


class DebianIndex:
    """Debian 的 Packages 索引 + （按需下载的）Contents 索引。"""

    def __init__(self, arch, cache):
        self.arch = arch
        self.cache = cache
        self.packages = {}   # name → DebPackage
        self._contents = None
        # main 先加载，security 后加载并覆盖同名条目 —— security 的版本永远 ≥ main，
        # 这样 libssl3 之类拿到的就是打过补丁的版本。
        self._load_packages(DEBIAN_MIRROR, DEBIAN_SUITE, required=True)
        self._load_packages(DEBIAN_SECURITY, DEBIAN_SUITE + "-security", required=False)

    def _load_packages(self, mirror, suite, required):
        url_base = "%s/dists/%s/main/binary-%s/Packages" % (mirror, suite, self.arch)
        dest_base = os.path.join(self.cache, "debian", "%s_%s_Packages" % (suite, self.arch))
        try:
            handle = download_index(url_base, dest_base)
        except Exception as exc:
            if required:
                raise
            # security 索引拿不到不该让整个构建失败：主仓库的库照样能用，
            # 只是少了安全更新。但必须显式喊出来，不能静默降级。
            log("::warning:: 取不到 %s 索引（%s），将只用主仓库的版本" % (suite, exc))
            return
        with handle as f:
            cur = {}

            def flush():
                if cur.get("Package") and cur.get("Filename"):
                    # security 后加载、版本更高，直接覆盖 main 的条目
                    self.packages[cur["Package"]] = DebPackage(
                        url="%s/%s" % (mirror, cur["Filename"]),
                        version=cur.get("Version", ""),
                        sha256=cur.get("SHA256"),
                        size=int(cur["Size"]) if cur.get("Size", "").isdigit() else None,
                    )
                cur.clear()

            for line in f:
                line = line.rstrip("\n")
                if not line:
                    flush()
                    continue
                for key in ("Package", "Filename", "Version", "SHA256", "Size"):
                    prefix = key + ": "
                    if line.startswith(prefix):
                        cur[key] = line[len(prefix):]
                        break
            flush()
        log("已加载 %s 索引：%d 个包" % (suite, len(self.packages)))

    def _load_contents(self):
        """Contents 索引把【文件路径】映射到包名，用来兜底解析未知 soname。

        约 40MB，所以只有 SONAME_TO_DEB 查不到时才下载。
        """
        if self._contents is not None:
            return
        self._contents = {}
        url_base = "%s/dists/%s/main/Contents-%s" % (DEBIAN_MIRROR, DEBIAN_SUITE, self.arch)
        dest_base = os.path.join(self.cache, "debian", "Contents-%s" % self.arch)
        with download_index(url_base, dest_base) as f:
            for line in f:
                if ".so" not in line:
                    continue
                parts = line.rsplit(None, 1)
                if len(parts) != 2:
                    continue
                filepath, pkgs = parts[0].strip(), parts[1].strip()
                base = os.path.basename(filepath)
                if ".so" not in base:
                    continue
                # "libs/libssl3,libs/libssl3t64" → 取第一个的最后一段
                pkg = pkgs.split(",")[0].split("/")[-1]
                self._contents.setdefault(base, pkg)
        log("已加载 Contents 索引：%d 个 .so 条目" % len(self._contents))

    def package_for_soname(self, soname):
        pkg = SONAME_TO_DEB.get(soname)
        if pkg and pkg in self.packages:
            return pkg, False
        self._load_contents()
        pkg = self._contents.get(soname)
        if pkg and pkg in self.packages:
            return pkg, True     # True = 表里没有，是自动查出来的
        return None, False

    def fetch(self, pkg, stage):
        p = self.packages[pkg]
        fname = os.path.basename(p.url)
        deb = download(p.url, os.path.join(self.cache, "debian", fname),
                       expected_sha256=p.sha256, expected_size=p.size)
        extract_deb(deb, stage)
        return p.version


def install_soname(src_roots, lib_dir, soname, elf_arch):
    """从 src_roots 里找出 SONAME 等于 soname 的那一个共享库，装进 lib_dir。

    【只装点名要的那一个】，不是把整棵树里的 .so 一扫而空 —— 后者踩过：
    libc6 的 deb 里带着几百个 gconv 编码模块（IBM*.so / ISO8859-*.so），
    再加上 MariaDB 插件被重复扫进来，lib/ 一下从 12MB 涨到 39MB 全是废物。
    按需安装还有个好处：装进来的每一个库都是闭包真正要求的，没有"以防万一"。

    落地时按 SONAME 命名并平铺到一个目录：ld.so 的 --library-path 只认目录列表，
    平铺一个目录就够；符号链接一律解引用成真实文件，不去赌 upk 打包对链接的处理。
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


def ensure_pkg_stage(index, pkg, cache):
    """把 Debian 包解到缓存目录，返回该目录。已经解好的直接复用。

    两个坑都在这里堵住：

    1. 缓存目录【必须带上架构】。早前只用包名做键，结果先跑的 arm64 把
       stage-deb/libc6 填成了 aarch64 的库，后跑的 amd64 直接复用，
       于是死在"libc6 里没有 ld-linux-x86-64.so.2"。

    2. 【解到一半的目录不能当成解好的】。extract_deb 一进门就 makedirs，
       所以上一次在解包中途失败也会留下一个目录；用 isdir 判断"要不要下载"
       就会把这个残缺目录当成完整的，症状同样是"包里没有某个 soname"，
       指不到真因上。改成解到 .tmp 再原子改名 —— 只有完整解完的才叫数。
    """
    stage = os.path.join(cache, "stage-deb", index.arch, pkg)
    if os.path.isdir(stage):
        return stage
    tmp = stage + ".tmp"
    shutil.rmtree(tmp, ignore_errors=True)
    index.fetch(pkg, tmp)
    os.makedirs(os.path.dirname(stage), exist_ok=True)
    os.rename(tmp, stage)
    return stage


def resolve_closure(out_dir, elf_arch, index, cache, mariadb_stage, max_rounds=8):
    """反复解析 DT_NEEDED 闭包，缺什么就补什么，直到不再缺。

    补的来源有先后：先在 MariaDB 自己的解包树里找（它自带的库版本要和 mariadbd
    配套），找不到才去 Debian 仓库拉。
    """
    lib_dir = os.path.join(out_dir, "lib")
    roots = [os.path.join(out_dir, "bin"),
             os.path.join(out_dir, "sbin"),
             os.path.join(out_dir, "lib", "plugin")]
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
            if install_soname([mariadb_stage], lib_dir, soname, elf_arch):
                log("  %s ← MariaDB 自带" % soname)
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
            stage = ensure_pkg_stage(index, pkg, cache)
            if not install_soname([stage], lib_dir, soname, elf_arch):
                raise SystemExit(
                    "Debian 包 %s 里没有 soname 为 %s 的库 —— "
                    "soname→包的映射错了，检查 SONAME_TO_DEB" % (pkg, soname))
            log("  %s ← %s" % (soname, pkg))
            progressed = True
        if not progressed:
            raise SystemExit("闭包无法收敛：这一轮一个新库都没装上，检查上面的日志")

    raise SystemExit("闭包在 %d 轮内没有收敛" % max_rounds)


def assemble(stage, out_dir, arch):
    """从解包出来的 Debian 树里挑出我们要的东西，组装成最终目录布局。

    布局（安装到 NAS 后是 <安装目录>/mariadb/）：
        sbin/mariadbd
        bin/{mariadb,mariadb-dump,mariadb-check,mariadb-upgrade}
        lib/*.so*          ← 含 ld.so 的完整运行库闭包（平铺）
        lib/plugin/*.so    ← MariaDB 服务端插件
        share/             ← charsets / errmsg.sys / bootstrap 用的 .sql
    """
    elf_arch = ARCH_ELF[arch]
    shutil.rmtree(out_dir, ignore_errors=True)
    for sub in ("sbin", "bin", "lib/plugin", "share"):
        os.makedirs(os.path.join(out_dir, sub), exist_ok=True)

    src_sbin = os.path.join(stage, "usr/sbin/mariadbd")
    if not os.path.exists(src_sbin):
        raise SystemExit("没找到 mariadbd：%s" % src_sbin)
    shutil.copy2(src_sbin, os.path.join(out_dir, "sbin/mariadbd"))
    os.chmod(os.path.join(out_dir, "sbin/mariadbd"), 0o755)

    for name in KEEP_BIN:
        src = os.path.join(stage, "usr/bin", name)
        if not os.path.exists(src):
            raise SystemExit("没找到可执行文件 %s（上游包内容变了？）" % name)
        dst = os.path.join(out_dir, "bin", name)
        shutil.copy2(src, dst)
        os.chmod(dst, 0o755)

    src_plugin = os.path.join(stage, "usr/lib/mysql/plugin")
    if not os.path.isdir(src_plugin):
        raise SystemExit("没找到插件目录：%s" % src_plugin)
    for name in sorted(os.listdir(src_plugin)):
        if name in SKIP_PLUGINS or not name.endswith(".so"):
            continue
        src = os.path.join(src_plugin, name)
        if os.path.isfile(src) and not os.path.islink(src):
            dst = os.path.join(out_dir, "lib/plugin", name)
            shutil.copy2(src, dst)
            os.chmod(dst, 0o755)

    # share：只留 bootstrap 与运行必需的，其余语言的 errmsg.sys 全部丢掉
    src_share = os.path.join(stage, "usr/share/mariadb")
    if not os.path.isdir(src_share):
        raise SystemExit("没找到 share 目录：%s" % src_share)
    dst_share = os.path.join(out_dir, "share")
    shutil.copytree(os.path.join(src_share, "charsets"),
                    os.path.join(dst_share, "charsets"))
    os.makedirs(os.path.join(dst_share, "english"), exist_ok=True)
    shutil.copy2(os.path.join(src_share, "english/errmsg.sys"),
                 os.path.join(dst_share, "english/errmsg.sys"))
    for sql in ("mariadb_system_tables.sql", "mariadb_performance_tables.sql",
                "mariadb_system_tables_data.sql", "fill_help_tables.sql",
                "maria_add_gis_sp_bootstrap.sql", "mariadb_sys_schema.sql"):
        shutil.copy2(os.path.join(src_share, sql), os.path.join(dst_share, sql))

    # 注意：这里【不】把 stage 里的 .so 一扫而空。运行库一律由 resolve_closure
    # 按 DT_NEEDED 精确按需安装（MariaDB 自带的优先、Debian 兜底）。
    return elf_arch


def verify(out_dir, arch):
    """构建的最后一道闸：架构对不对、loader 在不在、闭包还缺不缺。"""
    elf_arch = ARCH_ELF[arch]
    loader = ARCH_LOADER[arch]
    problems = []

    mariadbd = os.path.join(out_dir, "sbin/mariadbd")
    e = elfdeps.ELF(mariadbd)
    if e.arch != elf_arch:
        problems.append("mariadbd 架构是 %s，应为 %s" % (e.arch, elf_arch))

    loader_path = os.path.join(out_dir, "lib", loader)
    if not os.path.exists(loader_path):
        problems.append("缺少动态加载器 lib/%s —— 没有它整个包在沙箱里跑不起来" % loader)

    resolved, missing = elfdeps.closure(
        [os.path.join(out_dir, "lib")],
        [os.path.join(out_dir, "bin"), os.path.join(out_dir, "sbin"),
         os.path.join(out_dir, "lib/plugin")])
    if missing:
        problems.append("依赖闭包仍有缺口：%s" % ", ".join(sorted(missing)))

    for path, elf in elfdeps.iter_elf([out_dir]):
        if elf.arch != elf_arch:
            problems.append("架构不符：%s 是 %s" % (
                os.path.relpath(path, out_dir), elf.arch))

    if problems:
        for p in problems:
            print("::error:: %s" % p, file=sys.stderr)
        raise SystemExit("校验未通过")
    log("校验通过：%s，闭包 %d 个库，loader=%s" % (elf_arch, len(resolved), loader))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--arch", required=True, choices=["amd64", "arm64"])
    ap.add_argument("--mariadb-version", required=True)
    ap.add_argument("--out", required=True, help="输出目录，如 rootfs_arm64/mariadb")
    ap.add_argument("--cache", default=None, help="下载缓存目录")
    args = ap.parse_args()

    cache = args.cache or os.path.join(
        os.path.expanduser("~"), ".cache", "mariadb-ugreen-app")
    stage = os.path.join(cache, "stage-mariadb-%s-%s" % (args.mariadb_version, args.arch))
    shutil.rmtree(stage, ignore_errors=True)

    fetch_mariadb(args.mariadb_version, args.arch, cache, stage)
    elf_arch = assemble(stage, args.out, args.arch)
    index = DebianIndex(args.arch, cache)
    _resolved, auto_found = resolve_closure(args.out, elf_arch, index, cache, stage)
    verify(args.out, args.arch)

    if auto_found:
        log("提示：以下 soname 是靠 Contents 索引兜底找到的，"
            "建议补进 fetch-runtime.py 的 SONAME_TO_DEB 加速后续构建：")
        for so, pkg in sorted(auto_found.items()):
            log('    "%s": "%s",' % (so, pkg))

    total = sum(os.path.getsize(os.path.join(r, f))
                for r, _d, fs in os.walk(args.out) for f in fs)
    log("完成：%s（%.1f MB）" % (args.out, total / 1024.0 / 1024.0))


if __name__ == "__main__":
    main()
