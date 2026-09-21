# 安全策略

## 报告漏洞

请**不要**通过公开 Issue 报告安全问题。

请使用 GitHub 的
[Private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
功能提交，或直接联系仓库维护者。

报告时请尽量包含：

- 受影响的版本或 commit
- 复现步骤
- 影响范围（泄露凭据？越权？拒绝服务？）
- 你建议的修复方向（如果有）

我们会在确认后尽快回复，并在修复发布后于 CHANGELOG 中致谢（除非你希望匿名）。

## 本项目的敏感数据

tgup 会在本机保存三类敏感信息，**任何一项泄露都等同于账号被接管或行为被追溯**：

| 数据 | 默认位置 | 泄露后果 |
|---|---|---|
| `api_id` / `api_hash` | 配置文件或环境变量 | 他人可以你的应用身份调用 Telegram API |
| 会话文件 | `~/.tgup/tgup-go.session` | 直接冒用你的登录态，无需验证码 |
| 上传台账 | `~/.tgup/ledger.json` | 暴露本机文件路径与已发消息 id |

因此：

- **不要把 `api_hash`、session 文件、台账提交到仓库。**
  仓库里跟踪的是不含凭据的模板 `tgup.example.yaml`，实际使用的 `tgup.yaml`
  已在 `.gitignore` 中忽略。
- 优先用环境变量而不是配置文件：

  ```sh
  export TG_API_ID=123456
  export TG_API_HASH=0123456789abcdef0123456789abcdef
  ```

- 若必须写进配置文件，请 `chmod 600`。程序检测到该文件对其他用户可读时
  会打印警告，但那只是提醒，不会自动修正权限。
- 会话文件所在目录以 `0700` 创建，台账以 `0600` 写入。

## 如果你怀疑凭据已经泄露

1. **重置 api_hash**：到 <https://my.telegram.org/apps> 重新生成。
   api_hash 无法单独吊销，重新生成即让旧值失效。
2. **终止会话**：在 Telegram 客户端「设置 → 设备」里踢掉不认识的登录设备，
   并删除本机的 session 文件。
3. 检查 git 历史。如果凭据曾经被提交过，仅仅删除文件是不够的——
   它仍在历史里，需要重写历史（`git filter-repo`）或直接作废该凭据。

## 不在范围内的内容

- Telegram 服务端的行为与其风控策略
- 因用户自行设置过高的 `--rate` / 过小的 `--gap-min` 而触发的账号限制
  （本项目提供限速与占空比控制，但最终发送量由使用者决定）
- 本机已被攻破的场景
