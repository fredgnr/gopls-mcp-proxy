# Homebrew 发布维护

`fredgnr/homebrew-tap` 中的 `Formula/gopls-mcp-proxy.rb` 安装主仓库已发布的二进制。用户通过 `brew install fredgnr/tap/gopls-mcp-proxy` 安装，通过 `brew update` 和 `brew upgrade fredgnr/tap/gopls-mcp-proxy` 获取新版。

## 一次性凭证配置

主仓库 Actions 自带的 `GITHUB_TOKEN` 只能写入主仓库。自动更新 tap 需要单独创建一个 fine-grained personal access token，并把它保存为主仓库的 Actions secret：

1. 在 GitHub 的 **Settings → Developer settings → Personal access tokens → Fine-grained tokens** 创建 token，Resource owner 选择 `fredgnr`。
2. Repository access 选择 **Only select repositories**，只勾选 `homebrew-tap`。
3. Repository permissions 将 **Contents** 设为 **Read and write**；Metadata 的只读权限由 GitHub 自动提供。该流程通过 GitHub Contents API 向 tap 的 `main` 分支提交配方，无需 Pull requests 或 Workflows 写权限。
4. 在主仓库的 [New repository secret](https://github.com/fredgnr/gopls-mcp-proxy/settings/secrets/actions/new) 页面新增 secret，名称为 `HOMEBREW_TAP_TOKEN`，值为刚创建的 token。

选择合适的有效期，并在到期前轮换。不要把 token 写进配方、脚本、仓库文件或聊天。提交工作流文件不会自动创建这个 secret；必须由有权限的维护者完成设置。

tap 的默认分支使用 `main`。如果以后为该分支增加必须通过 PR 才能修改的规则，需要同步调整发布方式，使配方更新符合分支保护要求。

## 自动发布流程

主仓库的 `.github/workflows/release.yml` 在 GitHub Release 发布任务成功后调用 `.github/workflows/homebrew.yml`。Homebrew 工作流也支持手工 `workflow_dispatch`，用于首次同步和独立重试，不依赖新的 `release` 事件触发。PR 验证复用相同的安装测试，并设置 `publish: false`，不会更新 tap。

仅 `vMAJOR.MINOR.PATCH` 形式的稳定 tag 进入 tap。开发版、预发布版、带构建元数据的版本不会作为稳定配方发布。实际 Release 还必须已经公开，且不是 draft 或 prerelease。

工作流按顺序完成：

1. 下载对应 Release 中四个目标平台的压缩包和 `checksums.txt`，确认附件齐全并验证实际内容的 SHA-256。候选版本高于 `v0.6.0` 时，同样下载并验证该固定升级基线的四个平台附件。
2. 使用主仓库的 `scripts/homebrew/gopls-mcp-proxy.rb.in` 生成候选配方，下载地址始终指向具体版本。
3. 在 macOS arm64、macOS amd64、Linux amd64 和 Linux arm64 runner 上检查配方、安装并执行 MCP 冒烟测试。测试覆盖代理版本输出、默认 gopls 后端、初始化、非空工具列表和连接关闭；精简 PATH 用于验证安装入口对依赖的定位。干净安装验证并卸载后，再安装 `v0.6.0` 基线、将测试 tap 更新到候选配方并执行真正的 `brew upgrade`，确认相同稳定命令路径报告候选版本，重跑 MCP 冒烟测试并卸载。候选版本不高于基线时，只验证干净安装。
4. 全部验证通过后，使用 `HOMEBREW_TAP_TOKEN` 更新 tap 中的配方。同步前再次读取当前配方版本；旧版本不能覆盖新版，同一版本的发布校验值不能被更换。提交时携带读取到的文件 blob SHA，由 GitHub Contents API 进行有条件的原子更新。如果期间有人修改了该配方，提交失败，必须检查并重试，不会覆盖并发变更。没有内容变化时不创建重复提交。

macOS/Linux 的原生运行检查验证发布包能够在对应架构上运行；它们不代替用户在实际 IDE、实际项目和现有 MCP 配置中的验收。包内仍不包含 gopls，配方声明 `gopls` 与 Go 运行依赖，主仓库原有真实后端互通测试继续独立执行。

## 首次同步与重试

首次可以使用已发布的 `v0.7.0`。完成凭证设置，并确保 Homebrew 工作流已经合入主仓库默认分支后：

1. 打开主仓库的 [Homebrew 工作流](https://github.com/fredgnr/gopls-mcp-proxy/actions/workflows/homebrew.yml)。
2. 选择 **Run workflow**，分支选择 `main`，`tag` 填入 `v0.7.0` 或需要同步的已发布稳定版本。`publish` 默认勾选；取消勾选可以只运行全部验证，不更新 tap，也不需要跨仓库写入凭证。
3. 等待配方生成、四平台测试和 tap 更新完成，再检查 tap 默认分支中的版本与校验值。

如果 GitHub Release 已经发布，但 Homebrew 同步失败，只重跑失败的 Homebrew job，或通过上面的独立工作流重试。不要为了同步 tap 重建已发布附件，也不要重跑整套 Release 发布来替换文件。程序变更应使用新的版本号；同版本的配方修正必须保留原有发布包的校验值。

凭证缺失、过期、未授权目标仓库或目标分支不允许提交时，先修正配置再重试。并发修改引发的 SHA 冲突需要先检查 tap 中的新配方，再重新运行工作流。平台测试失败时先查看对应 job 的日志，修复配方模板或发布包问题。已发布版本的文件保持不变；如果是程序或打包问题，发布新稳定版本。

## 安装与升级验收

干净环境中执行：

```sh
brew install fredgnr/tap/gopls-mcp-proxy
brew test fredgnr/tap/gopls-mcp-proxy
"$(brew --prefix)/bin/gopls-mcp-proxy" --version 2>&1
```

`--version` 写入 stderr，因此示例使用 `2>&1`。配方测试包含真实 MCP 交互，不能仅以版本号或帮助输出通过作为功能验收。

Homebrew 工作流在四个平台执行 `v0.6.0 → 候选版本` 的安装与升级验证；首次候选 `v0.7.0` 对应真实的 `v0.6.0 → v0.7.0` 升级。该测试使用同一套配方生成和附件校验流程，并检查升级前后的稳定命令入口及升级后的 MCP 交互。

下一稳定版本发布并成功同步 tap 后，仍应在保留旧版安装的实际机器上执行：

```sh
brew update
brew upgrade fredgnr/tap/gopls-mcp-proxy
"$(brew --prefix)/bin/gopls-mcp-proxy" --version 2>&1
```

确认版本已改变、MCP 配置仍指向稳定的 `$(brew --prefix)/bin/gopls-mcp-proxy`，再重新建立 MCP 连接，验证实际项目可初始化并列出工具。配置文件中的路径需填写命令展开后的绝对路径；TOML/JSON 不会自动执行 `$(brew --prefix)`。CI 的基线升级测试不代表未来一次真实发版事件、跨仓库凭证、用户端 `brew update` 和实际 MCP 宿主重连这一整条流程已经验收。

## 修改配方

配方模板由主仓库维护；修改模板后应运行 Homebrew 脚本测试，并对已发布版本执行完整 Homebrew 工作流。直接修改 tap 中自动生成的配方会在后续同步时被模板覆盖。

```sh
python3 -B -m unittest discover -s scripts/tests -p 'test_homebrew*.py' -v
```

tap 自身的 `.github/workflows/test.yml` 也会验证提交到 tap 的配方。当前项目没有声明许可证，不应为通过审核而替项目编造 `MIT`、`BSD` 等许可字段；若以后正式选择许可证，再按实际许可更新配方。
