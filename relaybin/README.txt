wnrelay 的各平台二进制（手机端中转程序，USB 模式按 App 区分流量用），由 build.sh 或
scripts/build-release.sh 在构建时生成并嵌入主程序，不提交到仓库。
直接 go build / go run 时这里没有二进制，启用弱网会退回直连模式（无法按 App 分类）。
