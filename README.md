# 云点传话筒（Yundian Chuanhuatong）

> 老师在手机浏览器上发一句话，教室电脑立刻**全屏大字 + 语音播报**。服务端跑在 Cloudflare Workers（Durable Objects / D1 / KV / Workers AI），教室端是 Windows 单文件 exe（Go + WebView2，界面内嵌）。

## 功能

- **教师端 H5**（手机/电脑浏览器）：卡密注册开通、班级管理、教室端绑定码、即时喊话、定时喊话（周计划/一次性）、发送记录与「已播报」回执
- **教室端 exe**（Windows）：配对码绑定、WebSocket 实时接收（断线自动降级 HTTP 轮询）、全屏大字、本机 TTS 播报（云端 TTS 兜底）、播报回执、托盘常驻/开机自启/热键、发现新版本提示
- **管理后台**：用户管理（禁用/启用、续期、改套餐、删除、重置密码）、卡密批量生成（3 天体验 / 1 年卡）、卡密列表
- **商业规则**：卡密开通、班级名额硬限制、到期硬拦（手动发送与定时任务两个入口都拦）、关键操作留痕

## 架构

```
教师端 H5 ──HTTPS──> Worker ──> D1 (users/classes/messages/schedules/codes/logs/sessions)
                        │  └─ Workers AI：TTS 合成（云端兜底），音频缓存 KV
                        │
教室端 exe ──WebSocket /api/device/ws──> Durable Object Room(classId)
                        ▲
定时喊话：DO Alarm → 每 60s 自唤醒 → POST /internal/tick?key=CRON_KEY → 到点推送
```

- 每个班级一个 Durable Object（Room），教师端与教室端都按 `classId` 进房
- 定时任务由 DO Alarm 驱动，不依赖 cron 触发器（兼容免费版配额）
- 教室端优先本机 SAPI/PlaySound 播报，失败时回退云端合成音频；断线期间的消息不补发

## 目录

```
server/            Worker 源码
  src/index.js       全部 HTTP/WS 路由 + 业务逻辑
  src/room.js        Durable Object（房间、设备广播、Alarm 定时）
  public/index.html  教师端 H5（单文件内联 CSS+JS）
  public/admin.html  管理后台
  schema.sql         建表脚本
  wrangler.toml      绑定配置（★生产标识/密钥已替换为占位符）
device-go/         教室端 Go 源码（WebView2 内嵌壳，界面 go:embed 进 exe）
  ui/index.html      教室端界面（单文件内联 CSS+JS）
  pack.py            打包脚本（go build → 单文件 exe）
  make_icon.py       图标生成（png/ico → windres .syso）
tools/             构建与运维脚本
  deploy_run.py      部署（wrangler deploy）
  d1.py / migrate.py 远程 D1 执行 / 迁移
  mkadmin.py         生成/重置后台管理员口令（PBKDF2-SHA256）
  build_font_subset.py  按页面用字裁剪自托管字体
```

## 本地开发

```bash
# 1) 启动本地 Worker（本地 D1 落在 device-go/.state）
cd server
npx -y wrangler@4 dev --local --port 8799 --persist-to ../device-go/.state

# 2) 首次建表（另开终端）
cd server
npx -y wrangler@4 d1 execute DB --local --persist-to ../device-go/.state \
  -c wrangler.toml --file schema.sql
```

- 教师端 H5：`http://127.0.0.1:8799/`
- 管理后台：`http://127.0.0.1:8799/admin.html`
- 教室端源码调试（Windows）：在 `device-go/` 下设置 `YDC_BASE=http://127.0.0.1:8799` 后 `go run .`

## 部署

1. 替换 `server/wrangler.toml` 里的占位符：`account_id`、`routes`、`database_id`、KV `id`、`PUBLIC_BASE`
2. 建表：`npx -y wrangler@4 d1 execute <db> --remote --file schema.sql`
3. 写入定时任务密钥（推荐用 secret，而不是明文 `[vars]`）：`npx -y wrangler@4 secret put CRON_KEY`
4. 部署：`python tools/deploy_run.py`（等价于 `cd server && npx -y wrangler@4 deploy`）

凭据通过环境变量 `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` 提供，或放在仓库根 `.secrets/cf_token.txt`（该目录已被 `.gitignore` 忽略）。

## 打包教室端 exe

```bash
cd device-go
python pack.py     # 产出 dist/云点传话筒.exe（单文件，界面已内嵌）
```

需要 Go 1.22+ 与 Windows 构建环境。图标资源（`.syso`）由 `make_icon.py` 生成，仓库不包含二进制产物。

## 关于本仓库

- 公开版**仅包含核心源码**。未包含：exe 构建产物、运营/买家/部署文档、开发记忆、本地回归脚本，以及任何生产凭据与基础设施 ID
- `server/wrangler.toml` 中所有生产标识与密钥均为占位符，部署前请替换为你自己的值
- 界面字体为「站酷快乐体」（SIL OFL 1.1）；仓库只包含按页面用字裁剪后的 woff2 子集
- 本项目未附开源许可证；如需二次分发或商用，请先与作者联系
