#!/usr/bin/env python3
"""纯标准库的 ELF 依赖分析器（DT_NEEDED / DT_SONAME / PT_INTERP）。

为什么自己写：开发机是 macOS，没有 readelf/objdump（otool 只认 Mach-O），
而构建脚本必须能在 macOS 上判断"这堆 Linux 二进制还缺哪些 .so"。
只依赖 struct，不需要任何第三方包。

用法：
    python3 elfdeps.py needed <file>...        # 每行一个 soname
    python3 elfdeps.py soname <file>...        # 每行 "<路径> <soname>"
    python3 elfdeps.py interp <file>           # 打印 PT_INTERP
    python3 elfdeps.py arch <file>             # x86_64 / aarch64 / ...
    python3 elfdeps.py closure <libdir>... -- <root-file>...
        # 从 root 出发递归解析，输出：
        #   OK   <soname>  <解析到的路径>
        #   MISS <soname>            ← 闭包缺口，构建脚本据此再去下载
"""

import struct
import sys
import os

# e_machine → 架构名（只列我们会碰到的）
EM = {0x3E: "x86_64", 0xB7: "aarch64", 0x28: "arm", 0x14: "ppc", 0x15: "ppc64"}

PT_INTERP = 3
PT_DYNAMIC = 2
DT_NULL = 0
DT_NEEDED = 1
DT_STRTAB = 5
DT_SONAME = 14
DT_RUNPATH = 29
DT_RPATH = 15


class NotELF(Exception):
    pass


class ELF:
    def __init__(self, path):
        self.path = path
        with open(path, "rb") as f:
            self.data = f.read()
        d = self.data
        if len(d) < 64 or d[:4] != b"\x7fELF":
            raise NotELF(path)
        self.is64 = d[4] == 2
        little = d[5] == 1
        self.end = "<" if little else ">"
        if not self.is64:
            raise NotELF("%s: 只支持 64 位 ELF" % path)
        (self.e_type, self.e_machine) = struct.unpack_from(self.end + "HH", d, 16)
        (self.e_phoff,) = struct.unpack_from(self.end + "Q", d, 32)
        (self.e_phentsize, self.e_phnum) = struct.unpack_from(self.end + "HH", d, 54)

    @property
    def arch(self):
        return EM.get(self.e_machine, "machine-0x%x" % self.e_machine)

    def _phdrs(self):
        for i in range(self.e_phnum):
            off = self.e_phoff + i * self.e_phentsize
            # Elf64_Phdr: type, flags, offset, vaddr, paddr, filesz, memsz, align
            p_type, _flags, p_offset, p_vaddr, _paddr, p_filesz, _memsz, _align = \
                struct.unpack_from(self.end + "IIQQQQQQ", self.data, off)
            yield p_type, p_offset, p_vaddr, p_filesz

    def interp(self):
        for p_type, p_offset, _vaddr, p_filesz in self._phdrs():
            if p_type == PT_INTERP:
                return self.data[p_offset:p_offset + p_filesz].rstrip(b"\0").decode()
        return None

    def _vaddr_to_off(self, vaddr):
        """把虚拟地址映射回文件偏移（用 PT_LOAD 段的映射关系）。"""
        PT_LOAD = 1
        for p_type, p_offset, p_vaddr, p_filesz in self._phdrs():
            if p_type == PT_LOAD and p_vaddr <= vaddr < p_vaddr + p_filesz:
                return p_offset + (vaddr - p_vaddr)
        return None

    def _dynamic(self):
        """返回 (tag, value) 列表 + 字符串表的文件偏移。"""
        dyn_off = dyn_size = None
        for p_type, p_offset, _vaddr, p_filesz in self._phdrs():
            if p_type == PT_DYNAMIC:
                dyn_off, dyn_size = p_offset, p_filesz
                break
        if dyn_off is None:
            return [], None
        entries = []
        strtab_vaddr = None
        for i in range(dyn_size // 16):
            tag, val = struct.unpack_from(self.end + "qQ", self.data, dyn_off + i * 16)
            if tag == DT_NULL:
                break
            if tag == DT_STRTAB:
                strtab_vaddr = val
            entries.append((tag, val))
        strtab_off = self._vaddr_to_off(strtab_vaddr) if strtab_vaddr is not None else None
        return entries, strtab_off

    def _str(self, strtab_off, idx):
        if strtab_off is None:
            return None
        start = strtab_off + idx
        end = self.data.index(b"\0", start)
        return self.data[start:end].decode()

    def needed(self):
        entries, strtab = self._dynamic()
        return [self._str(strtab, v) for t, v in entries if t == DT_NEEDED]

    def soname(self):
        entries, strtab = self._dynamic()
        for t, v in entries:
            if t == DT_SONAME:
                return self._str(strtab, v)
        return None

    def runpath(self):
        entries, strtab = self._dynamic()
        for t, v in entries:
            if t in (DT_RUNPATH, DT_RPATH):
                return self._str(strtab, v)
        return None


def iter_elf(paths):
    """遍历文件/目录，产出所有能解析的 ELF。"""
    for p in paths:
        if os.path.isdir(p):
            for root, _dirs, files in os.walk(p):
                for name in files:
                    fp = os.path.join(root, name)
                    if os.path.islink(fp):
                        continue
                    try:
                        yield fp, ELF(fp)
                    except (NotELF, OSError, struct.error, ValueError):
                        continue
        else:
            try:
                yield p, ELF(p)
            except (NotELF, OSError, struct.error, ValueError):
                continue


def build_index(libdirs):
    """soname → 真实路径。同名以先出现的目录为准。"""
    index = {}
    for d in libdirs:
        for fp, e in iter_elf([d]):
            so = e.soname()
            for key in filter(None, (so, os.path.basename(fp))):
                index.setdefault(key, fp)
    return index


def closure(libdirs, roots):
    """从 roots 出发递归解析 DT_NEEDED，返回 (resolved, missing)。"""
    index = build_index(libdirs)
    resolved = {}
    missing = set()
    queue = []
    for r in roots:
        for _fp, e in iter_elf([r]):
            queue.extend(e.needed())
    seen = set()
    while queue:
        so = queue.pop()
        if so in seen:
            continue
        seen.add(so)
        path = index.get(so)
        if path is None:
            missing.add(so)
            continue
        resolved[so] = path
        try:
            queue.extend(ELF(path).needed())
        except (NotELF, OSError, struct.error, ValueError):
            pass
    return resolved, missing


def main(argv):
    if len(argv) < 2:
        print(__doc__)
        return 2
    cmd = argv[1]
    args = argv[2:]

    if cmd == "needed":
        out = set()
        for _fp, e in iter_elf(args):
            out.update(e.needed())
        for so in sorted(out):
            print(so)
    elif cmd == "soname":
        for fp, e in iter_elf(args):
            print(fp, e.soname() or "-")
    elif cmd == "interp":
        for fp, e in iter_elf(args):
            print(fp, e.interp() or "-")
    elif cmd == "arch":
        for fp, e in iter_elf(args):
            print(fp, e.arch, "exec" if e.e_type == 2 else "dyn" if e.e_type == 3 else e.e_type)
    elif cmd == "runpath":
        for fp, e in iter_elf(args):
            print(fp, e.runpath() or "-")
    elif cmd == "closure":
        if "--" not in args:
            print("用法: closure <libdir>... -- <root>...", file=sys.stderr)
            return 2
        i = args.index("--")
        resolved, missing = closure(args[:i], args[i + 1:])
        for so in sorted(resolved):
            print("OK  ", so, resolved[so])
        for so in sorted(missing):
            print("MISS", so)
        return 1 if missing else 0
    else:
        print("未知子命令: %s" % cmd, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
