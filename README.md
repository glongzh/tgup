# tgup - Telegram 视频批量上传工具

**把「尽快传完」改成「长期低调地传完」。**

用个人账号（MTProto / gotd）把本地视频批量上传到 Telegram。核心不是跑满带宽，
而是在数天到数周的时间尺度上，把上行流量整形得像正常使用——限速、占空比、
时间窗、日配额、断点续传、超限切割，全都为这一个目标服务。

```
[Unreleased]  <!-- TODO: 首次打 tag 后换成 CI 徽章与版本徽章 -->
```

> **尚未发布正式版本。** 模块路径目前是占位的 `github.com/glongzh/tgup`，
> 建好远端仓库后请一并替换（见 `go.mod`、`.goreleaser.yaml`）。

---

## 目录

- [它解决什么问题](#它解决什么问题)
- [功能](#功能)
- [安装](#安装)
- [快速开始](#快速开始)
- [安全须知](#安全须知)
- [常用参数](#常用参数)
- [配置文件](#配置文件)
- [与 Python 版的兼容性](#与-python-版的兼容性)
- [工作原理](#工作原理)
- [开发](#开发)
- [故障排查](#故障排查)
- [许可证](#许可证)

## 它解决什么问题

大批量往 Telegram 传视频时，最容易踩的坑不是「传得慢」，而是**传得太快**：

- 持续满速上行会让运营商侧的家宽/PCDN 判定模型把你标记成异常节点，
  轻则限速，重则被要求整改。
- Telegram 侧对短时间内的密集上传也有自己的风控。

tgup 的做法是把上传当作一个**长期后台任务**来调度，而不是一次传输任务：
自己给自己设限速、设占空比（传一段歇一段）、只在指定时段传、每天传够就停，
并且在实测吞吐相对自身峰值塌陷时自动降速冷却。所有状态落盘，随时可以
Ctrl-C，重跑即续。

## 功能

| 能力 | 说明 |
|---|---|
| 目录扫描 | 递归收集视频文件，按名称 / 大小 / 修改时间排序，跳过隐藏目录与上次的分片目录 |
| 令牌桶限速 | `--rate 4Mbps`，把上行压在物理带宽的一个比例上 |
| 占空比 | `--burst 20m --rest 10m`，传一段停一段，带随机抖动 |
| 时间窗 | `--window 09:00-23:30`，支持跨零点、可重复指定 |
| 日配额 | `--daily-cap 15GB`，`--on-cap stop` 退出或 `wait` 等到次日 |
| 限速自检 | 实测吞吐相对**自身峰值**塌陷时冷却降速，恢复后上浮 |
| 断点续传 | 分片级进度落盘（`~/.tgup/state/`，6h TTL） |
| 上传台账 | 已传文件 + 每日用量（`~/.tgup/ledger.json`），跨天续传的唯一真相来源 |
| 多连接上传 | `--connections 4`，把分片摊到多条 TCP；跨洋高 RTT 链路上这是决定速度的主因 |
| 超限无损切割 | 超过 2GB / 4GB 的文件先 `ffmpeg -c copy` 切段，传完自动清理 |
| 视频属性与缩略图 | ffprobe 探测时长分辨率（内联播放按钮），ffmpeg 抽帧并避开黑帧 |

## 安装

### 从源码

```sh
git clone https://github.com/glongzh/tgup
cd tgup
make build          # 产物在 ./tgup，版本信息已注入
```

或者：

```sh
go install github.com/glongzh/tgup@latest
```

### 预编译二进制

从 [Releases](https://github.com/glongzh/tgup/releases) 下载对应平台压缩包解压即可。
支持 linux / macOS / windows，amd64 与 arm64。

### 依赖

需要 **ffmpeg** 与 **ffprobe**（用于超限切割、时长分辨率探测、缩略图抽帧）：

```sh
# Debian / Ubuntu
sudo apt install ffmpeg
# Arch
sudo pacman -S ffmpeg
# macOS
brew install ffmpeg
# Windows
winget install Gyan.FFmpeg
```

没有 ffmpeg 也能用，但超限文件会被跳过、视频没有播放按钮和缩略图。

### Telegram API 凭据

到 <https://my.telegram.org/apps> 申请 `api_id` 与 `api_hash`，然后：

```sh
export TG_API_ID=123456
export TG_API_HASH=0123456789abcdef0123456789abcdef
```

首次运行会交互式登录（手机号 + 验证码 + 可选两步验证），会话存到
`~/.tgup/tgup-go.session`。

## 快速开始

```sh
# 先看计划，不真传
tgup --to me -r --plan ~/Videos

# 限 4Mbps，传 20 分钟歇 10 分钟，只在 09:00-23:30，每天最多 15GB
tgup --to @mychannel -r \
    --rate 4Mbps --burst 20m --rest 10m \
    --window 09:00-23:30 --daily-cap 15GB \
    --video ~/Videos

# 看有哪些会话可以作为 --to
tgup --list-chats

# 断点续传：Ctrl-C 之后重跑同一条命令即可
```

## 安全须知

三类数据等同于账号控制权，**任何一项都不要提交到仓库**：

| 数据 | 默认位置 | 泄露后果 |
|---|---|---|
| `api_id` / `api_hash` | 配置文件或环境变量 | 他人可以你的应用身份调用 Telegram API |
| 会话文件 | `~/.tgup/tgup-go.session` | 直接冒用登录态，无需验证码 |
| 上传台账 | `~/.tgup/ledger.json` | 暴露本机文件路径与已发消息 id |

- 仓库跟踪的是不含凭据的模板 `tgup.example.yaml`；实际使用的 `tgup.yaml`
  已在 `.gitignore` 中忽略。
- 优先用环境变量提供凭据。若写进文件，请 `chmod 600`——程序检测到该文件
  对其他用户可读时会打印警告，但不会自动修正权限。
- 会话目录以 `0700` 创建，台账以 `0600` 写入。

完整的漏洞报告流程见 [SECURITY.md](SECURITY.md)。

## 常用参数

完整列表：`tgup --help`。

### 目标与输入

| 参数 | 说明 |
|---|---|
| `--to` | `me` / `@username` / 数字 id（频道用 `--list-chats` 输出的 `-100` 前缀 id） |
| `-r, --recursive` | 递归子目录 |
| `--ext` | 扩展名白名单，逗号分隔 |
| `--min-size` / `--max-size` | 按大小过滤 |
| `--sort` | `name` / `size` / `mtime`（默认），`--reverse` 倒序 |

### 上行整形

| 参数 | 说明 |
|---|---|
| `--rate` | 速率，如 `4Mbps` / `500KB/s`。**唯一控制限速的字段**。小写 `b` = 比特，大写 `B` = 字节 |
| `--burst` / `--rest` | 时长，如 `20m` / `10m`。**必须成对出现** |
| `--window` | 允许时段 `HH:MM-HH:MM`，可重复；支持跨零点 |
| `--daily-cap` | 每日上限，如 `15GB` |
| `--on-cap` | `stop`（默认）退出 / `wait` 等到次日 |
| `--gap-min` / `--gap-max` | 文件之间的随机间隔（秒） |

> `burst` / `rest` / `cooldown` 控制的是「传多久、歇多久」的**时长**，不是速率。
> 填错字段程序会直接指出，而不是笼统报错。

### 传输

| 参数 | 说明 |
|---|---|
| `--connections` | 到 DC 的 TCP 连接数，默认 4，上限 8 |
| `--concurrency` | 并发在途分片数，默认 4，应 ≥ `connections` |
| `--retries` | 失败重试次数，默认 5，必须 ≥ 1 |
| `--part-size` | 分片大小（KB），默认按文件大小自适应 |
| `--no-resume` | 关闭分片级续传 |
| `--state-dir` / `--ledger` | 状态与台账位置 |
| `--force` | 忽略台账重复上传 |

### 发送

| 参数 | 说明 |
|---|---|
| `--video` | ffprobe 探测属性，支持内联播放（推荐） |
| `--photo` / `--force-document` | 按照片发送 / 按普通文件发送 |
| `--no-thumb` / `--thumb-at` | 关闭缩略图 / 指定取帧位置（`10%` 或 `90s`） |
| `--caption` / `--name-caption` | 说明文字 / 用文件名当说明文字 |
| `--parse-mode` | `md`（默认）/ `html` / `none` |
| `--ttl` / `--silent` | 定时自毁 / 静默发送 |

### 连接

| 参数 | 说明 |
|---|---|
| `--proxy` | `socks5://user:pass@host:1080` 或 `http://host:8080`（仅 `socks5` / `http`，不支持 `socks4`） |
| `--session` | 会话文件路径 |
| `--session-string` | 一次性导入 Telethon StringSession |
| `--verbose` | 打印 gotd 全部网络日志 |

### 动作

| 参数 | 说明 |
|---|---|
| `--plan` | 只打印计划与耗时预估 |
| `--list-chats[=N]` | 列出会话后退出 |
| `--dump-config` | 输出带注释的 YAML 模板 |
| `--version` | 打印版本信息 |

## 配置文件

支持 YAML / TOML / JSON，优先级为
**命令行显式参数 > 配置文件（含 profile）> 内置默认值**。

```sh
cp tgup.example.yaml tgup.yaml
chmod 600 tgup.yaml
tgup --from-config tgup.yaml --profile night ~/Videos
```

- 相对路径按**配置文件所在目录**解析。
- 支持任意层级的分节写法；未知键会报错并给出拼写建议。
- `profiles:` 段定义命名档位，用 `--profile <名字>` 叠加。
- `tgup --dump-config > tgup.yaml` 可导出带注释的当前配置。

## 与 Python 版的兼容性

本仓库是 `tgup.py` 的 Go 重写版。命令行参数同名同义，状态文件互通。

| 事项 | 说明 |
|---|---|
| 会话文件 | Telethon 的 SQLite session 无法复用。首次运行交互登录，或用 `--session-string` 一次性导入现有 StringSession |
| 台账 / 续传 | **完全兼容**（`ledger.json` / `state/` 字节级同格式），两个版本可以中途互换、不会重复上传。由 `TestUploadStateFileFormat` 守住 |
| 代理 | 支持 socks5 / http CONNECT；socks4 不再支持（`x/net` 无对应实现，会明确报错而不是静默直连） |
| 跨 DC 目标 | 目标实体在其他 DC 时明确报错（Telethon 会自动迁移，gotd 不内建）。传到 Saved Messages 或本账号常用目标不受影响 |
| 分片超限边界 | 文件介于 4000×512KB 与 2GiB（或 8000×512KB 与 4GiB）之间时，Python 版会整批崩溃；Go 版跳过该文件继续 |
| `--list-chats` | 频道/群组以 `-100` 前缀标记 id 输出，可直接用作 `--to` |

## 工作原理

```
main
 ├─ resolveArgs      三层参数合并 + 启动期校验
 ├─ discover         扫描 → 过滤 → 确定性排序
 ├─ openLedger       台账命中过滤
 └─ run
     ├─ parse*       量纲解析（rate / burst / window / daily-cap…）
     ├─ newPacer     令牌桶 + 占空比 + 时间窗 + 日配额 + 限速自检
     └─ client.Run
         ├─ 登录 / premium 检测（决定 2GB 还是 4GB 上限）
         ├─ resolveTarget
         └─ 逐文件
             ├─ sendOne       ≤ 上限：分片上传 → sendMedia
             └─ sendInParts   > 上限：ffmpeg -c copy 切段 → 逐段 sendOne
```

关键设计取舍：

- **`Pacer.Acquire` 在派发分片之前调用**，所以令牌桶天然成为整个上传流程的节流阀，
  而不是事后统计。
- **限速自检以「自身实测峰值」为基准**，不是以 `--rate` 为基准。
  后者在 `--rate` 设得高于链路实际能力时会永远误判「被限速」，
  越跑越慢（这是旧实现的实际故障模式）。
- **实测吞吐的分母会扣掉自己造成的静默**（限速休眠、占空比、时间窗），
  否则会把自己的节流误判成对端限速。
- **长静默（> 120s）时主动收掉附加连接**，醒来再重建，避免占着 TCP 空转。
- **错误分级**：单个文件的切割失败 / 超限 / 配额用尽只跳过该文件，
  不让整批崩掉；只有真正的致命错误才向上抛。

## 开发

```sh
make check          # 与 CI 一致：gofmt 检查 + go vet + go test
make test-race      # 竞态检测（需要 CGO）
make cover          # 覆盖率
make lint           # golangci-lint
make fuzz           # 短时模糊测试
make build          # 带版本信息编译
```

需要真实视频的用例会用 ffmpeg 现场生成素材；环境里没有 ffmpeg 会自动跳过。

贡献流程与代码约定见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 故障排查

**「实测上传峰值远低于 `--rate`」**
瓶颈不在限速器，而在链路或连接数。跨洋高 RTT 链路单条 TCP 的吞吐受 RTT
和丢包制约，先加大 `--connections`；或者把 `--rate` 调到接近实测值以免误判。

**缩略图是黑的**
片头通常是黑帧。程序默认在 10%/30%/50%/70% 四个位置抽帧并挑第一个非黑帧，
仍不满意可以用 `--thumb-at 1.5m` 指定时间点。

**上传完没有播放按钮**
加 `--video`。没有 ffprobe 时探测会失败，文件会以普通文件形式发送。

**提示「服务端已丢弃先前上传的分片」**
续传状态超过 6 小时（`STATE_TTL`）后服务端多半已回收分片。程序会自动作废
状态并整份重传一次，属于预期行为。

**目标在别的 DC 上报错**
gotd 不内建自动迁移。改用 `@username`，或选本账号常用目标。

**Windows 上提示找不到 ffmpeg**
确认 `ffmpeg` / `ffprobe` 在 `PATH` 里，`ffmpeg -version` 能跑通。

## 许可证

[MIT](LICENSE)

## 致谢

- [gotd/td](https://github.com/gotd/td) — MTProto 客户端库
- [Telethon](https://github.com/LonamiWebs/Telethon) — 台账与续传状态格式与之兼容
- [FFmpeg](https://ffmpeg.org/) — 切割、探测与抽帧

> 本项目与 Telegram 官方无关。请遵守 Telegram 服务条款与你所在地区的法律法规，
> 并自行承担使用风险。
