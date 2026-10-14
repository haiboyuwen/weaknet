# 内置共享 CA

把 CA 身份文件 `ca.pem`（同时含证书和私钥）放在本目录后再编译，程序会内置这张 CA：
所有电脑运行同一份程序时共用同一张证书，App 的 debug 包只需打包一次。

- 沿用已有证书：复制本机 CA 目录里的 `ca.pem`（macOS `~/Library/Application Support/weaknet/ca.pem`，
  Windows `%AppData%/weaknet/ca.pem`）。已打包该证书的 App 无需改动。
- `ca.pem` 已被 `.gitignore` 忽略，**不要提交到仓库**：拿到它的人能解密信任该证书的 debug 包流量。
  编译出的程序同样含有私钥，只发给需要的人。
- 本目录没有 `ca.pem` 时，程序按原方式在每台电脑首次启动时生成自己的 CA。
