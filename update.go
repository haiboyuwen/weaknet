package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	npmPackage      = "weaknet"
	maxReleaseBytes = 256 << 20
)

var releaseBase = "https://github.com/haiboyuwen/weaknet/releases"

// 官方源在国内可能很慢，失败时再查镜像。
var npmRegistries = []string{"https://registry.npmjs.org", "https://registry.npmmirror.com"}

// runCommand 处理子命令，返回 false 表示不是子命令，按原方式启动模拟器。
func runCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "-v", "--version":
		fmt.Printf("weaknet version %s\n", version)
	case "update":
		method := ""
		for i := 1; i < len(args); i++ {
			switch {
			case args[i] == "--method" && i+1 < len(args):
				method = args[i+1]
				i++
			case strings.HasPrefix(args[i], "--method="):
				method = strings.TrimPrefix(args[i], "--method=")
			}
		}
		if err := runUpdate(method); err != nil {
			fmt.Fprintln(os.Stderr, "更新失败："+err.Error())
			os.Exit(1)
		}
	default:
		return false
	}
	return true
}

func runUpdate(method string) error {
	fmt.Printf("当前版本：%s\n正在检查更新...\n", version)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	latest, err := latestVersion(ctx)
	if err != nil {
		return fmt.Errorf("查询最新版本失败：%w", err)
	}
	fmt.Printf("最新版本：%s\n", latest)
	if canonicalVersion(version) == latest {
		fmt.Println("已是最新版本。")
		return nil
	}
	if method == "" {
		method = detectInstallMethod()
	}
	switch method {
	case "npm":
		// 通过 npm 的 weaknet 命令运行时，update 由 bin/wrapper.js 处理；这里是直接运行包内二进制的情况。
		if runtime.GOOS == "windows" {
			return fmt.Errorf("Windows 上运行中的程序无法被覆盖，请先退出模拟器，再执行：npm install -g %s@latest", npmPackage)
		}
		fmt.Printf("执行：npm install -g %s@latest\n", npmPackage)
		cmd := exec.CommandContext(ctx, "npm", "install", "-g", npmPackage+"@latest")
		cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
		return cmd.Run()
	case "self":
		backup, err := selfUpdate(ctx, latest)
		if err != nil {
			return err
		}
		fmt.Printf("已更新到 %s，旧版本保留在：%s\n", latest, backup)
		return nil
	default:
		return fmt.Errorf("未知的更新方式 %q，可用 npm 或 self", method)
	}
}

func canonicalVersion(v string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func latestVersion(ctx context.Context) (string, error) {
	var lastErr error
	for _, registry := range npmRegistries {
		v, err := fetchNpmVersion(ctx, registry)
		if err == nil {
			return canonicalVersion(v), nil
		}
		lastErr = err
	}
	return "", lastErr
}

func fetchNpmVersion(ctx context.Context, registry string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/"+npmPackage+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s 返回 HTTP %d", registry, resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if body.Version == "" {
		return "", fmt.Errorf("%s 未返回版本号", registry)
	}
	return body.Version, nil
}

// detectInstallMethod 二进制位于 npm 包内时用 npm 更新，否则下载 GitHub Release 替换自身。
func detectInstallMethod() string {
	path, err := os.Executable()
	if err != nil {
		return "self"
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return installMethodForPath(path)
}

func installMethodForPath(path string) string {
	if strings.Contains(filepath.ToSlash(path), "/node_modules/"+npmPackage+"/") {
		return "npm"
	}
	return "self"
}

// releaseAssetName 与 .github/workflows/release.yml 产出、bin/wrapper.js 使用的文件名一致。
func releaseAssetName(goos, goarch string) (string, error) {
	switch {
	case goos == "darwin" && (goarch == "amd64" || goarch == "arm64"):
		return "weaknet-darwin-" + goarch, nil
	case goos == "windows" && goarch == "amd64":
		return "weaknet-win32-x64.exe", nil
	case goos == "windows" && goarch == "arm64":
		return "weaknet-win32-arm64.exe", nil
	}
	return "", fmt.Errorf("没有 %s/%s 的发布版本", goos, goarch)
}

func selfUpdate(ctx context.Context, latest string) (string, error) {
	if _, err := releaseAssetName(runtime.GOOS, runtime.GOARCH); err != nil {
		return "", err
	}
	target, err := os.Executable()
	if err != nil {
		return "", err
	}
	if target, err = filepath.EvalSymlinks(target); err != nil {
		return "", err
	}
	return installFrom(ctx, target, latest)
}

// installFrom 下载 latest 版本、校验 SHA256SUMS 和程序信息后替换 target，返回旧版本的备份路径。
func installFrom(ctx context.Context, target, latest string) (string, error) {
	asset, err := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	archive := asset + ".gz"
	sums, err := download(ctx, fmt.Sprintf("%s/download/%s/SHA256SUMS", releaseBase, latest), 64<<10)
	if err != nil {
		return "", fmt.Errorf("下载 SHA256SUMS 失败：%w", err)
	}
	want, err := checksumFor(string(sums), archive)
	if err != nil {
		return "", err
	}
	fmt.Printf("下载 %s ...\n", archive)
	data, err := download(ctx, fmt.Sprintf("%s/download/%s/%s", releaseBase, latest, archive), maxReleaseBytes)
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("校验失败：%s 的 SHA-256 与 SHA256SUMS 不一致", archive)
	}
	staging, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(staging.Name())
	unpacked, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		staging.Close()
		return "", err
	}
	if _, err = io.Copy(staging, io.LimitReader(unpacked, maxReleaseBytes)); err != nil {
		staging.Close()
		return "", err
	}
	if err = staging.Chmod(0o755); err != nil && runtime.GOOS != "windows" {
		staging.Close()
		return "", err
	}
	if err = staging.Close(); err != nil {
		return "", err
	}
	if err = validateReleaseBinary(staging.Name()); err != nil {
		return "", err
	}
	return replaceExecutable(target, staging.Name())
}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s 超过大小上限", url)
	}
	return data, nil
}

// checksumFor 从 sha256sum 的输出（每行 "<hex>  <文件名>"）中找到 asset 的摘要。
func checksumFor(sums, asset string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset && len(fields[0]) == 64 {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中没有 %s", asset)
}

// validateReleaseBinary 不执行下载的程序，只检查它是本项目、且平台匹配的 Go 程序。
func validateReleaseBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("下载的文件不是 Go 程序：%w", err)
	}
	if info.Main.Path != "weaknet" && info.Path != "weaknet" {
		return fmt.Errorf("下载的文件不是 weaknet")
	}
	settings := map[string]string{}
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("下载的程序面向 %s/%s，本机是 %s/%s", settings["GOOS"], settings["GOARCH"], runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// replaceExecutable 把旧程序改名为备份，再把新程序放到原位置。Windows 不允许覆盖运行中的 exe，
// 但允许改名，所以两个平台都先改名再放入。
func replaceExecutable(target, downloaded string) (string, error) {
	backup := fmt.Sprintf("%s.old-%s", target, time.Now().Format("20060102150405"))
	if err := os.Rename(target, backup); err != nil {
		return "", fmt.Errorf("备份旧版本失败：%w", err)
	}
	if err := os.Rename(downloaded, target); err != nil {
		_ = os.Rename(backup, target)
		return "", fmt.Errorf("安装新版本失败：%w", err)
	}
	return backup, nil
}
