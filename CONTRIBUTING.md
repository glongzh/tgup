# 贡献指南

感谢你愿意花时间改进 tgup。这份文档说明本项目的约定，让改动更容易被合并。

## 环境准备

- Go：版本见 `go.mod`
- ffmpeg / ffprobe：切割超限文件与生成缩略图的用例会用到。
  没有也能跑测试——相关用例会自动跳过，但请在 PR 里说明你跳过了哪些。

```sh
git clone https://github.com/glongzh/tgup
cd tgup
go build ./...
go test ./...
```

## 提交前自检

```sh
make check     # = gofmt 检查 + go vet + go test，与 CI 一致
```

CI 还会在 Linux/macOS/Windows 三平台跑测试，并在 Linux 上跑 `-race`。
如果你的改动涉及并发（上传、限速器、台账），请本地也跑一次：

```sh
make test-race
```

## 代码约定

### 换行符必须是 LF

仓库通过 `.gitattributes` 强制 LF。`gofmt` 要求 LF，一旦某个文件带上 CRLF，
CI 的格式检查就会失败。Windows 用户请确认编辑器没有把换行符改成 CRLF。

### 错误处理

- 会被用户看到的失败，用 `fatal()` 并给出**可操作**的提示
  （告诉用户该改哪个参数、该装什么），而不是裸堆栈。
- 配置解析、参数合并这类需要写单测的逻辑，**返回 `error` 而不是 `fatal`**。
  `fatal` 内部是 `os.Exit`，会让这段逻辑无法测试。
- 错误分类用独立类型（`splitError` / `oversizeError` / `dailyCapError`），
  调用方用 `errors.As` 判断。单个文件失败不应该让整批崩掉。

### 外部命令

调用 ffmpeg / ffprobe 一律走 `ffRun(ctx, ...)`，`ctx` 从
`signal.NotifyContext` 一路传下来。**不要**在内部新建 `context.Background()`——
那会让 Ctrl-C 失效，而切割一个大文件可能要跑好几分钟。

### 并发

- 共享状态要么加锁，要么明确注释「仅主循环 goroutine 访问」。
- `Pacer` 里的 `muTok` / `muSt` 分别保护令牌桶与统计，注释里写明了各自的范围，
  改动时请保持这个划分。
- 新增并发路径后跑 `make test-race`。

### 兼容性

这是最容易被忽略、也最容易造成实际损失的一条：

- `~/.tgup/ledger.json` 与 `~/.tgup/state/*.json` 的**磁盘格式与 Python 版逐字节一致**，
  两个版本可以中途互换、不会重复上传。改动这两个文件的结构属于破坏性变更，
  必须在 CHANGELOG 里明确标注，并提供迁移方案。
- `TestUploadStateFileFormat` 就是为此存在的，它会比对精确的 JSON 字节。

### 注释

用中文写注释，风格是「说明**为什么**这么写」而不是「这行在做什么」。
踩过的坑、反直觉的取舍、被否掉的方案，都值得留一句——这些信息在 `git log` 里很容易丢失。

## 测试

- 需要真实视频的用例用 `makeTestVideo()` 现场生成，**不要**硬编码
  `/tmp/xxx.mp4` 这类路径（在 Windows 上等于永远不跑）。环境缺 ffmpeg 时用
  `t.Skip` 跳过，而不是让测试失败。
- 解析器（`parseRate` / `parseSize` / `parseWindow` / caption 实体）容易
  被畸形输入搞崩，新加的解析逻辑请补一条 fuzz 用例。
- 修 Bug 时请补一条**会在这个 Bug 存在时失败**的回归测试，并在注释里写明
  原来错在哪。仓库里已有若干这样的用例，可以参考。

## 提交信息

采用 [Conventional Commits](https://www.conventionalcommits.org/)：

```
feat: 支持按 --profile 叠加多档配置
fix: 递归扫描时隐藏目录未被跳过
docs: 补充代理配置说明
test: 为令牌桶补取消场景
```

`feat` / `fix` 会进入 changelog 分组。

## 提 PR

1. 从 `main` 切分支
2. `make check` 通过
3. PR 描述里写清楚：**改了什么、为什么、怎么验证的**
4. 涉及行为变更的，同步更新 README 与 CHANGELOG

## 安全

请不要在 Issue 或 PR 里贴出真实的 `api_hash`、session 文件内容或任何凭据。
发现安全问题的报告方式见 [SECURITY.md](SECURITY.md)。
