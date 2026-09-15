# 采集源配置（本地留存，不入库）

`sources.local.json` 被 .gitignore 排除，**不上传 GitHub**。
内容是 2026-09-15 实测可用的 14 个苹果 CMS V10 采集源（Mac + NAS 双端验证）。

## 重新导入（重装 upk / 新实例后）

    ./import-to-nas.sh          # 凭据从 ~/Desktop/"nas ssh.txt" 读，不写死

或手动：admin 登录 → 管理 → 配置文件 → 粘贴 local.json 全文 → 保存。
（必须走管理页"配置文件"或 /api/admin/config_file 接口才会合并生效；
/api/admin/config 只存原始串。）

## 换源

编辑 sources.local.json 的 api_site（键=英文标识，值={name, api, detail}）。
可用性判据：`GET <api>?ac=videolist&wd=片名` 返回 code:1 且有 list；
不要带 /at/xml 后缀（LunaTV 只解析 JSON）。候选源池：
https://github.com/waifu-project/movie/issues/45
