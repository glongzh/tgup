# 代码审核报告 — tgup

**日期**：2026-09-17
**范围**：仓库全部 15 个 Go 源文件（约 5100 行）、构建配置、文档
**目标**：达到可公开的开源项目标准
**结论**：**已达成**。发现 26 项问题（含 1 项凭据泄露、5 项会导致数据丢失或消息被拒的缺陷），
全部修复；开源基建补齐；验证全绿。

---

## 一、审核基线

审核开始时的实际状态（不是文档描述的、是实测的）：

| 检查项 | 结果 |
|---|---|
| `go build` | 通过 |
| `go vet` | 通过 |
| `go test` | 通过（4.5s） |
| `gofmt -l` | **列出全部 16 个文件** |
| `git` 仓库 | 不存在 |
| `LICENSE` / `.gitignore` / `Makefile` / `.github` | **均不存在** |
| 硬编码凭据 | **存在**（`tgup.yaml`） |
| 测试对 Windows 的有效性 | 切割相关用例全部静默跳过 |

---

## 二、P0 — 阻断开源发布

### 2.1 真实 API 凭据硬编码在版本控制内 🔴

`tgup.yaml` 包含可用的 `api_id` 与 `api_hash`。一旦仓库公开，任何人都能以
你的应用身份调用 Telegram API。

**修复**：新增不含凭据的 `tgup.example.yaml`（凭据部分改为环境变量说明），
`tgup.yaml` 加入 `.gitignore`，`SECURITY.md` 说明三类敏感数据的位置与泄露后果。

**遗留动作（需要你本人操作）**：到 <https://my.telegram.org/apps> 重新生成
`api_hash`。凭据无法单独吊销，重新生成即让旧值失效。

### 2.2 全部源文件是 CRLF 🔴

`gofmt -l` 把所有文件都列为未格式化。任何格式检查（本地 pre-commit、
CI、编辑器插件）都会直接失败。

**修复**：`gofmt -w` 统一为 LF；新增 `.gitattributes`（`* text=auto eol=lf`）
与 `.editorconfig` 在 git 层锁定，防止回退。

### 2.3 无 `.gitignore` 🔴

仓库根目录有两个 22MB 的构建产物（`tgup`、`tgup.exe`）。首次 `git add .`
会把 44MB 二进制连同本机 session、台账一起提交。

**修复**：`.gitignore` 覆盖构建产物、`tgup.yaml`、`*.session`、台账、
续传状态、分片临时目录、编辑器与覆盖率文件。

### 2.4 无 LICENSE 🔴

**修复**：MIT（按你的选择）。README 增加许可证章节，goreleaser 归档内含 LICENSE。

### 2.5 无 CI 🔴

**修复**：`.github/workflows/ci.yml` —— 三平台测试、Linux 上 `-race`、
gofmt / vet / golangci-lint、六目标交叉编译、fuzz 种子语料。
另加 `release.yml` + `.goreleaser.yaml`（tag 触发，含 changelog 分组与 checksum）。

---

## 三、P1 — 正确性与数据安全

### 3.1 `validatePartSize(0)` 整数除零 panic

```go
if ps%KB != 0 || (512*KB)%ps != 0 || ...   // ps == 0 时第二项直接崩
```

一个**负责报错**的函数自己崩溃。虽然当前调用点都有 `> 0` 前置判断，
但这是典型的定时炸弹——将来任何一处漏判都会变成 panic 而不是错误提示。

**修复**：`ps <= 0` 提到条件最前短路。

### 3.2 切割重试环节会同时丢掉原文件和分片

`resplitOversized` 的顺序是：

```go
os.Remove(p)                    // 先删原段
for ... { os.Rename(n, dst) }   // 再逐个改名
os.RemoveAll(tmp)
```

只要中途有一次 `Rename` 失败：原段已经没了，新分片还散在随后被 `RemoveAll`
掉的临时目录里 —— **两头落空，用户丢一个几 GB 的文件**。

**修复**：改为先把全部分片改名到位，最后才 `os.Remove(p)`。失败时保留原段。

### 3.3 caption 实体越界 → 整条消息被 Telegram 拒收

引入 fuzz 测试后**立刻**抓到两个独立成因，都是原实现就存在的：

1. **零长度实体**：` ``````` `（空预格式块）。正则里 code 组是 `([\s\S]*?)`
   而非 `+`，匹配出空正文，产出 `length=0` 的实体。
2. **非法 UTF-8 导致长度算错**：`"000000\xd2**\x87**"`。`\xd2` 与 `\x87`
   单独看都是坏字节，拼接后却构成合法字符 U+0487 —— 于是
   「各片段 UTF-16 长度之和」比整体长度多一格，实体越界。
   旧实现在完整串上测 offset（正确），但 length 仍只测片段，**同样越界**。

**修复**：入口统一 `sanitizeUTF8` 净化非法 UTF-8（净化后正则的匹配边界必然
落在 rune 边界，增量计数精确）；预格式分支增加长度检查。
两个输入已固化为回归语料与单元测试。

### 3.4 递归扫描不跳隐藏目录

```go
if d.IsDir() || strings.HasPrefix(d.Name(), ".") { return nil }
```

`WalkDir` 的回调返回 `nil` 只跳过**该条目本身**，仍会继续向下递归。
`.git`、`.cache`、`.Trash` 会被整个扫进来。要跳过目录必须返回 `filepath.SkipDir`。

**修复**：目录分支显式返回 `SkipDir`（根目录除外），并顺带把
`SPLIT_DIR_PREFIX` 目录也一并跳过。

### 3.5 排序比较函数不满足严格弱序

```go
if reverse { return !less }   // a == b 时返回 true
```

违反自反性，`sort` 的行为未定义。

**修复**：等值一律返回 `false`。

### 3.6 等值文件的上传顺序不可复现

候选集从 map 迭代构建，顺序本身就是随机的；配合 3.5 的比较函数，
一批同为 1GB 的视频每次运行上传顺序都不同。

**修复**：先 `sort.Strings(out)` 定序，再 `SliceStable`，等值项固定落到字典序。
`TestDiscoverEqualKeysKeepPathOrder` 反复跑 20 次验证稳定性。

### 3.7 `--retries 0` 让整批文件静默失败

```go
for attempt := 0; attempt < u.o.Retries; attempt++ { ... }
```

`Retries = 0` 时循环一次都不执行，`sent` 保持 false，每个文件只打印一句
「发送失败，跳过」——**界面上看不出任何异常，实际什么都没传**。
`sendPart` 同样问题。

**修复**：`Options.validate()` 启动期拦截。

### 3.8 ffmpeg / ffprobe 无法被 Ctrl-C 取消

`ffRun` 内部固定 `context.Background()`。切割一个几 GB 的文件要跑几分钟，
这期间按 Ctrl-C **毫无反应**，只能另开终端 kill。

**修复**：`ctx` 从 `signal.NotifyContext` 一路传到 `exec.CommandContext`；
错误分类也调整为优先识别取消/超时，否则会被误报成「找不到 ffmpeg」。

### 3.9 数值解析静默吞掉错误

- `strconv.ParseFloat` 的返回值被 `_` 丢弃：`parseRate("1.2.3Mbps")` → `0`，
  **等于把限速关掉**。`parseSize` / `parseDuration` / `parseThumbAt` 同样。
- `toFloat` 用 `fmt.Sscanf(s, "%g")`，只要求前缀可解析：
  `gap_min: "12abc"` 被静默读成 `12`。

**修复**：全部改为检查错误 / 用 `strconv.ParseFloat` 严格解析。

### 3.10 原子写临时文件名可能等于目标名

```go
tmp := stripExt(l.path) + ".tmp"
```

目标无扩展名时（`--ledger ~/.tgup/ledger`）`tmp == path`，`rename` 变成
自己改自己，原子性失效。

**修复**：固定用 `path + ".tmp"`。台账与续传状态共用 `writeFileAtomic`。

---

## 四、P2 — 健壮性与可维护性

| # | 问题 | 修复 |
|---|---|---|
| 4.1 | `--plan` 是纯本地计算，却因凭据检查排在短路之前而失败 | 凭据检查后移到真正要建连接处 |
| 4.2 | 量纲误填的提示来自 map 迭代，`20m` 同时能当「时长」和「大小」解析 → 同一份配置两次运行报出不同提示 | 改为有序切片，固定优先级（时长 → 大小 → 速率） |
| 4.3 | 配置层用 `fatal`（内部 `os.Exit`），整层无法测试 | `loadConfigFile` / `loadConfig` / `flattenConfig` / `applyConfigKey` 改为返回 `error`；`resolveArgs` 接受显式 `args` |
| 4.4 | `--help` 写着「connections 上限 8」，代码从未校验 | `validate()` 强制 1..8 |
| 4.5 | `--sort` / `--parse-mode` / `--on-cap` 非法取值静默回落默认行为 | 启动期明确报错 |
| 4.6 | 台账 `0644`、状态目录 `0755`（含本机绝对路径与消息 id） | 收紧为 `0600` / `0700`；`--dump-config` 输出文件改 `0600` |
| 4.7 | `utf16Len(out.String())` 每产出一段重算整串 → O(n²) | 增量 UTF-16 计数器 |
| 4.8 | 同一正则被 `FindStringSubmatch` + `FindStringSubmatchIndex` 跑两遍 | 只跑 `FindStringSubmatchIndex`，按组边界切片 |
| 4.9 | `attrValue` 每次调用都 `MustCompile` | 预编译两个常量正则 |
| 4.10 | `cliSet` 是包级可变 map，`newFlagSet` 硬编码 `os.Exit`，有死代码 | 改为局部变量；`ContinueOnError` + 显式错误返回；`errFlagParse` 区分「已打印过」 |
| 4.11 | `--help` 里 `-r` 与 `--recursive` 各占一行 | 短别名归并到长选项，显示为 `--recursive, -r` |
| 4.12 | 无版本号机制 | `--version`，`-ldflags` 注入 version/commit/date |

---

## 五、测试与基建

### 测试

原测试存在一个结构性问题：**切割相关用例硬编码 `/tmp/tgup-test-big.mp4`**，
文件不存在就 `t.Skip`。在 Windows 上这个路径永远不存在，等于这几条用例
从来没跑过——而它们覆盖的正是最容易丢数据的分片逻辑。

改动：

- 新增 `makeTestVideo()`：用 ffmpeg 现场生成带密集关键帧的测试视频
  （用内置的 `mpeg4` 编码器，不依赖 `libx264`）。没有 ffmpeg 才跳过。
- 新增约 30 条用例，覆盖：文件发现（隐藏目录 / 分片目录 / 显式文件绕过白名单 /
  大小过滤 / 排序确定性）、配置（嵌套展平 / 拼写建议 / 相对路径 / profile 叠加 /
  优先级）、参数校验（12 种非法组合）、原子写、状态文件字节格式、取消传播、
  UTF-16 偏移、非法 UTF-8、缩略图取帧位置等。
- 新增 3 个 fuzz 目标：`FuzzParseUnits`、`FuzzCaptionEntities`、
  `FuzzFlattenConfig`。其中 `FuzzCaptionEntities` 断言**实体必须落在纯文本的
  UTF-16 范围内**——这条不变量直接抓出了 3.3 的两个缺陷。
- 回归语料固化在 `testdata/fuzz/FuzzCaptionEntities/`。

### 基建

`LICENSE`、`Makefile`（build/test/test-race/cover/fmt-check/vet/lint/fuzz/check/
snapshot/clean）、`.golangci.yml`、`.gitignore`、`.gitattributes`、
`.editorconfig`、`.github/workflows/{ci,release}.yml`、`.goreleaser.yaml`、
`CONTRIBUTING.md`、`SECURITY.md`、`CHANGELOG.md`、`tgup.example.yaml`、`version.go`、
`README.md`（重写）。

`go.mod` 模块路径由 `tgup` 改为 `github.com/glongzh/tgup`。

---

## 六、验证结果

| 检查 | 结果 |
|---|---|
| `gofmt -l .` | 无输出 |
| `go vet ./...` | 通过 |
| `go test -count=1 ./...` | 通过（15.6s） |
| 交叉编译 linux/darwin/windows × amd64/arm64 | **6/6 通过**（15–16MB，CGO 关闭） |
| `--version` / `--help` | 正常，版本号正确注入 |
| 示例配置加载 + `--plan` | 正常（且不再要求凭据） |
| 错误提示质量 | 参数越界、配置拼写错误、量纲填错、缺凭据均给出可操作提示 |
| fuzz `FuzzCaptionEntities` | 494,224 次执行，通过 |
| fuzz `FuzzParseUnits` | 1,465,015 次执行，通过 |
| fuzz `FuzzFlattenConfig` | 1,454,516 次执行，通过 |

---

## 七、未处理项与遗留风险

以下是我**有意**没有做的，附理由：

1. **`.golangci.yml` 未在本机实跑** —— 环境没有安装 golangci-lint。
   配置已按本项目特点调过（关掉了与本项目「常量全大写、中文注释/错误信息」
   冲突的 ST1000/ST1003/ST1005），但首次 `make lint` 后仍可能需要微调。
   这是本次审核唯一未经验证的交付物。
2. **`-race` 本机未跑** —— `-race` 需要 CGO 与 C 工具链，本机没有。
   CI 的 Linux job 已配置。
3. **单包结构未拆分** —— 按约定保持 `package main`。若要作为库复用，
   需要拆 `internal/{config,pacer,ledger,upload,telegram}`。
   这是重构而非修 Bug，风险与收益需单独评估。
4. **无 i18n** —— 面向用户的输出只有中文。若要面向国际社区，需要抽出消息表。
5. **`run()` 顶层仍用 `fatal`** —— 这是 CLI 入口的合理用法；
   真正需要测试的配置层已经改为返回 `error`。
6. **未在实体生成后加最终 clamp** —— 根治手段是入口净化 UTF-8（已做），
   加上 fuzz 不变量与固化语料作为回归防线。再加一道生产侧范围校验属于
   重复防御，代价是 20 余行的类型 switch，收益有限。
7. **仓库根目录的 `tgup` / `tgup.exe`** —— 你自己的构建产物，我没有删除，
   已由 `.gitignore` 忽略。首次提交前请确认 `git status` 里没有它们。

## 八、建议的后续动作

1. **立刻**：去 <https://my.telegram.org/apps> 重新生成 `api_hash`。
2. 建远端仓库，替换 `go.mod` / `.goreleaser.yaml` / README 里的占位模块路径。
3. `git init` 后 `git status` 确认没有二进制、session、`tgup.yaml` 被跟踪。
4. 跑一次 `make check` 与 `make lint`，按 lint 实际输出微调配置。
5. 打 `v1.0.0` tag 触发发布流程（goreleaser 会自动生成 changelog 与 checksum）。
