# npm 分发与发布指南

weaknet 通过两个渠道分发，都由同一次 GitHub Actions 发布产出：

- **npm 包 `weaknet`**：`npm install -g weaknet` 安装，命令名 `weaknet`。npm 只是分发渠道，程序本身是 Go 编译的
  单文件，不依赖 Node；包里的 `bin/wrapper.js` 只负责按平台启动对应的二进制。
- **GitHub Releases**：每个平台一个 `.gz` 压缩的单文件，加 `SHA256SUMS` 校验文件，供不用 Node 的人下载，
  也供 `weaknet update` 自更新。

```text
git push 标签 v1.2.3
  └─ .github/workflows/release.yml
       ├─ prepare        校验标签格式（必须是 vX.Y.Z）
       ├─ verify         gofmt、go vet、go test、页面测试
       ├─ build ×4       各平台编译（先编译手机端中转程序嵌入），gzip 压缩
       ├─ github-release 生成 SHA256SUMS，创建 Release 并上传
       └─ npm            解压二进制放进 bin/，版本号改成 1.2.3，npm publish
```

版本号以 git 标签为准：`package.json` 里的 `version` 只是占位，发布时按标签改写；程序里的版本号
（`weaknet version`）也在编译时由标签写入，本地编译显示 `dev`。

## 一、首次发布前的一次性准备

### 1. npm 账号

用你发布 ccl 的那个 npm 账号即可，不需要新注册。包名 `weaknet` 目前没人占用，**第一次发布成功后包就归这个账号所有**。

1. 登录 <https://www.npmjs.com>。
2. 确认账号已开启双重验证（头像 → Account → Two-Factor Authentication）。npm 现在要求发布账号开启 2FA。
3. 确认包名仍可用：`npm view weaknet` 返回 `404 Not Found` 即可用。

### 2. 创建发布用的 Access Token

GitHub Actions 里没人能输入 2FA 验证码，所以首次发布要用一个 Token：

1. npmjs.com → 头像 → **Access Tokens** → **Generate New Token** → **Granular Access Token**。
2. 填写：
   - **Token name**：比如 `weaknet-github-actions`。
   - **Bypass two-factor authentication**：勾选（否则 CI 发布会因为要求 2FA 而失败）。
   - **Expiration**：选最短够用的期限。有写权限的 Token 有效期有上限，过期后需重新生成。
   - **Packages and scopes → Permissions**：选 **Read and write**。
   - **Select packages**：包还不存在，没法只选 `weaknet`，选 **All packages**。首次发布后建议换成下面的
     Trusted Publishing，再删除这个 Token。
3. 生成后**立刻复制** Token（只显示一次）。

### 3. 把 Token 存进 GitHub 仓库 Secret

在仓库 `haiboyuwen/weaknet` → **Settings → Secrets and variables → Actions → New repository secret**：

- Name：`NPM_TOKEN`
- Secret：粘贴上一步的 Token

或用命令行（会提示粘贴，不会留在 shell 历史里）：

```bash
gh secret set NPM_TOKEN --repo haiboyuwen/weaknet
```

**不要**设置 `WEAKNET_CA_PEM`。它会把 CA 私钥编译进公开发布的程序，任何人都能取出来。
团队共用证书请在「设置 → 导入 CA」导入同一份 `ca.pem`，导入后保存在本机，升级不会丢失。

### 4. 确认 Actions 可以创建 Release

仓库 **Settings → Actions → General**：

- **Actions permissions**：允许运行 Actions。
- **Workflow permissions** 保持默认（只读）即可。`release.yml` 在需要的任务里单独申请了
  `contents: write`（创建 Release）和 `id-token: write`（npm provenance / Trusted Publishing）。

## 二、发布一个新版本

1. 确认 `main` 上的 CI（`.github/workflows/ci.yml`）是绿的，要发布的提交都已推送。
2. 打标签并推送，版本号用 `vX.Y.Z`：

   ```bash
   git checkout main && git pull
   git tag v1.0.0
   git push origin v1.0.0
   ```

3. 看发布进度：

   ```bash
   gh run watch --repo haiboyuwen/weaknet     # 或在仓库 Actions 页面查看 Release and Publish
   ```

4. 发布完成后检查：

   ```bash
   npm view weaknet version                   # 应为 1.0.0
   npm install -g weaknet && weaknet version  # 应为 weaknet version v1.0.0
   gh release view v1.0.0 --repo haiboyuwen/weaknet
   ```

   Release 里应有 4 个 `.gz`（`weaknet-darwin-amd64`、`weaknet-darwin-arm64`、`weaknet-win32-x64.exe`、
   `weaknet-win32-arm64.exe`）、`SHA256SUMS`、`LICENSE.txt` 和 `THIRD_PARTY_NOTICES.txt`。

### 重新运行与幂等

- 某个任务失败（比如网络问题）时，在 Actions 页面点 **Re-run failed jobs** 即可：Release 已存在时只刷新文件，
  npm 上已有该版本时跳过发布。
- npm 同一个版本号**只能发布一次**，删除后也不能重用。发错了就发一个新的补丁版本（如 `v1.0.1`），
  必要时用 `npm deprecate weaknet@1.0.0 "请升级到 1.0.1"` 提示用户。
- 标签打错且还没发布成功时，可以删掉重打：

  ```bash
  git push --delete origin v1.0.0 && git tag -d v1.0.0
  ```

## 三、首次发布后：改用 Trusted Publishing（推荐）

Trusted Publishing 让 npm 只信任这个仓库的这个工作流，不再需要长期有效的 Token（ccl 用的就是这种方式）：

1. npmjs.com → 进入包 `weaknet` → **Settings** → **Trusted Publisher** → 选 **GitHub Actions**，填写：
   - **Organization or user**：`haiboyuwen`
   - **Repository**：`weaknet`
   - **Workflow filename**：`release.yml`
   - **Environment**：留空
2. 删除仓库 Secret `NPM_TOKEN`，并在 npmjs.com 撤销那个 Token：

   ```bash
   gh secret delete NPM_TOKEN --repo haiboyuwen/weaknet
   ```

   工作流不用改：没有 `NPM_TOKEN` 时，`npm publish` 自动通过 GitHub 的 OIDC 身份发布。
3. 可选：包 **Settings → Publishing access** 选择要求 2FA 并禁止 Token 发布，进一步防止 Token 泄露后被冒用。

## 四、用户侧：安装、更新与兼容性

```bash
npm install -g weaknet     # 安装
weaknet                    # 启动模拟器（参数同直接运行程序，如 weaknet -ui-port 9000）
weaknet version            # 查看版本
weaknet update             # 更新到最新版本
```

- **Node 版本**：`bin/wrapper.js` 只用 ES5 语法，Node 8 及以上都能运行；包里没有任何依赖，也没有安装脚本
  （不会在安装时下载东西），离线或内网 npm 镜像都能装。
- **支持平台**：macOS（Apple 芯片 / Intel）、Windows（x64 / ARM64）。其他平台安装时 npm 会提示不支持。
- **国内网络**：可以用镜像安装，`npm install -g weaknet --registry=https://registry.npmmirror.com`。
  镜像通常几分钟内同步新版本；`weaknet update` 查询版本时官方源失败会自动改查这个镜像。
- **`weaknet update` 怎么更新**：
  - npm 安装的：执行 `npm install -g weaknet@latest`。Windows 上运行中的程序文件无法被覆盖，
    **请先退出正在运行的模拟器**再更新，否则 npm 会报文件被占用（EBUSY / EPERM）。
  - 直接下载的单文件：从 GitHub Release 下载新版本，按 `SHA256SUMS` 校验并确认是本项目、本平台的程序后替换自身，
    旧版本保留为同目录下的 `*.old-时间` 备份。
- **Ctrl+C / 关闭窗口**：npm 启动方式下，包装脚本会等模拟器恢复完手机代理再退出，行为和直接运行程序一致。

## 五、常见问题

| 现象 | 原因与处理 |
| --- | --- |
| `npm publish` 报 `ENEEDAUTH` / `E401` | 没有 `NPM_TOKEN` 且还没配置 Trusted Publishing；或 Token 已过期。重新生成 Token 更新 Secret。 |
| `E403 ... two-factor authentication` | 生成 Token 时没勾选 Bypass 2FA。重新生成并勾选。 |
| `E403 ... You do not have permission` | Token 没有写权限，或包已被别的账号占用。 |
| `cannot publish over the previously published versions` | 该版本已发布过。工作流会自动跳过；要发新内容请用新标签。 |
| `provenance` 相关报错 | provenance 要求仓库公开且任务有 `id-token: write`，`release.yml` 已配置；仓库改为私有时需去掉 `--provenance`。 |
| `prepare` 任务报标签格式错误 | 标签必须是 `v1.2.3` 这样的三段版本号。 |
| 用户 `weaknet update` 提示查询失败 | 用户网络访问不了 npm 官方源和 npmmirror，可让其手动 `npm install -g weaknet@latest --registry=...`。 |
| macOS 提示无法打开 / 已损坏 | 程序未签名。终端执行一次 `xattr -d com.apple.quarantine <程序路径>`，或在「系统设置 → 隐私与安全性」允许。 |

## 相关文件

| 文件 | 作用 |
| --- | --- |
| `package.json` | npm 包信息：命令名、包含的文件、Node 版本要求、支持平台 |
| `bin/wrapper.js` | `weaknet` 命令入口，按平台启动二进制，处理 npm 方式的 `update` |
| `scripts/build-release.sh` | 编译一个平台（含手机端中转程序），CI 和本地 `build.sh` 共用 |
| `.github/workflows/release.yml` | 推送标签后的发布流程 |
| `.github/workflows/ci.yml` | 推送 main / PR 时的检查 |
| `update.go` | `weaknet version` / `weaknet update`（直接下载的单文件的自更新） |
