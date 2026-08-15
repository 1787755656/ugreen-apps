#!/usr/bin/env python3
"""从 Docker Hub 上按【manifest digest】把上游镜像的指定文件解出来。

为什么不用 `docker pull` + `docker export`：
  - 构建机上不一定有 Docker（也不该为了取几个文件装一个）；
  - digest 是内容寻址的，等于天然的校验和 —— 上游重新推同一个 tag 时这里会
    直接下不到，而不是【静默地跟着换了内容】。所以脚本里只钉 digest，不钉 tag。

每个 blob 都会用 manifest 里给的 digest 复核一遍，对不上直接退出。

用法：
    extract-image.py <repo> <manifest-digest> <缓存目录> <输出目录> <路径前缀>...

路径前缀是 tar 里的成员名前缀（不带开头的 ./），命中就解出来。
层是按顺序叠加的，后面的层覆盖前面的 —— 和 Docker 自己的语义一致。
"""

import hashlib
import io
import json
import os
import sys
import tarfile
import urllib.request

REGISTRY = "https://registry-1.docker.io"
AUTH = "https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull"

ACCEPT = ",".join([
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def get(url, token, accept=None):
    req = urllib.request.Request(url)
    req.add_header("Authorization", "Bearer " + token)
    if accept:
        req.add_header("Accept", accept)
    with urllib.request.urlopen(req, timeout=180) as r:
        return r.read()


def token_for(repo):
    with urllib.request.urlopen(AUTH % repo, timeout=60) as r:
        return json.load(r)["token"]


def verify(data, digest):
    got = "sha256:" + hashlib.sha256(data).hexdigest()
    if got != digest:
        raise SystemExit("校验和不符：期望 %s，实际 %s" % (digest, got))


def cached_blob(repo, digest, token, cache):
    path = os.path.join(cache, digest.replace(":", "_"))
    if os.path.exists(path):
        data = open(path, "rb").read()
        if "sha256:" + hashlib.sha256(data).hexdigest() == digest:
            return data
    print("    下载 %s…" % digest[7:19], flush=True)
    data = get("%s/v2/%s/blobs/%s" % (REGISTRY, repo, digest), token)
    verify(data, digest)
    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.replace(tmp, path)
    return data


def wanted(name, prefixes):
    name = name.lstrip("./")
    return any(name == p or name.startswith(p.rstrip("/") + "/") or name.startswith(p)
               for p in prefixes)


def main():
    if len(sys.argv) < 6:
        raise SystemExit(__doc__)
    repo, digest, cache, out = sys.argv[1:5]
    prefixes = sys.argv[5:]

    os.makedirs(cache, exist_ok=True)
    os.makedirs(out, exist_ok=True)

    token = token_for(repo)
    manifest_raw = get("%s/v2/%s/manifests/%s" % (REGISTRY, repo, digest), token, ACCEPT)
    verify(manifest_raw, digest)
    manifest = json.loads(manifest_raw)

    layers = manifest["layers"]
    print("  %d 层" % len(layers), flush=True)

    found = 0
    for layer in layers:
        blob = cached_blob(repo, layer["digest"], token, cache)
        mode = "r:gz" if layer["mediaType"].endswith("gzip") else "r:"
        with tarfile.open(fileobj=io.BytesIO(blob), mode=mode) as tf:
            for member in tf:
                if not wanted(member.name, prefixes):
                    continue
                # 白出文件（.wh.xxx）表示"上层把它删了"—— 我们只取固定几个路径，
                # 碰到就当没这个文件，别把 .wh. 前缀的空文件解出来。
                base = os.path.basename(member.name)
                if base.startswith(".wh."):
                    target = os.path.join(out, os.path.dirname(member.name.lstrip("./")),
                                          base[4:])
                    if os.path.exists(target):
                        os.remove(target)
                    continue
                member.name = member.name.lstrip("./")
                if member.issym() or member.islnk():
                    # 软链在打包时会被 ugcli 解引用成完整拷贝，我们自己按 SONAME
                    # 命名真实文件（见 build.sh），所以这里直接跳过链接项。
                    continue
                tf.extract(member, out, set_attrs=False)
                if member.isfile():
                    found += 1
    print("  解出 %d 个文件" % found, flush=True)
    if found == 0:
        raise SystemExit("一个文件都没解出来 —— 镜像的目录结构大概是变了")


if __name__ == "__main__":
    main()
