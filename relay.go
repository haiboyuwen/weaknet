package main

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// relaybin 由 build.sh / scripts/build-release.sh 生成，不提交到仓库：wnrelay-arm64 / wnrelay-arm / wnrelay-amd64
//
//go:embed all:relaybin
var relayFS embed.FS

const relayRemotePath = "/data/local/tmp/wnrelay"

// relayOffset 中转程序转发到电脑所用的手机端端口 = 代理端口 + relayOffset
const relayOffset = 10000

func relayBinary(abi string) ([]byte, string) {
	arch := map[string]string{"arm64-v8a": "arm64", "armeabi-v7a": "arm", "armeabi": "arm", "x86_64": "amd64"}[abi]
	if arch == "" {
		return nil, ""
	}
	b, err := relayFS.ReadFile("relaybin/wnrelay-" + arch)
	if err != nil {
		return nil, arch
	}
	return b, arch
}

// startRelay 把中转程序推到手机并启动，成功后手机上 127.0.0.1:<port> 由中转程序监听，
// 转发到手机端 <port+relayOffset>，再经 adb reverse 到电脑上的 <port>
func (a *App) startRelay(serial string, lane *Lane) error {
	abi, err := a.runAdb(serial, "shell", "getprop", "ro.product.cpu.abi")
	if err != nil {
		return fmt.Errorf("读取 CPU 架构失败：%v", err)
	}
	bin, arch := relayBinary(strings.TrimSpace(abi))
	if bin == nil && arch != "" {
		return fmt.Errorf("此版本未内置 %s 中转程序（请用 build.sh 构建）", arch)
	}
	if bin == nil {
		return fmt.Errorf("没有适用于 %s 的中转程序", abi)
	}
	tmp, err := os.CreateTemp("", "wnrelay-*")
	if err != nil {
		return err
	}
	tmp.Write(bin)
	tmp.Close()
	defer os.Remove(tmp.Name())
	a.runAdb(serial, "shell", "pkill", "-f", relayRemotePath+" -listen 127.0.0.1:"+strconv.Itoa(lane.port))
	if out, err := a.runAdb(serial, "push", tmp.Name(), relayRemotePath); err != nil {
		return fmt.Errorf("推送中转程序失败：%s", out)
	}
	if out, err := a.runAdb(serial, "shell", "chmod", "755", relayRemotePath); err != nil {
		return fmt.Errorf("chmod 失败：%s", out)
	}
	relayPort := lane.port + relayOffset
	if out, err := a.runAdb(serial, "reverse", fmt.Sprintf("tcp:%d", relayPort), fmt.Sprintf("tcp:%d", lane.port)); err != nil {
		return fmt.Errorf("adb reverse 失败：%s", out)
	}

	args := []string{"-s", serial, "shell", relayRemotePath,
		"-listen", fmt.Sprintf("127.0.0.1:%d", lane.port), "-upstream", fmt.Sprintf("127.0.0.1:%d", relayPort)}
	cmd := exec.Command(a.adb, args...)
	hideWindow(cmd)
	stdin, _ := cmd.StdinPipe() // 保持打开：本程序退出时 stdin 关闭，中转程序随之退出
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		first := true
		for sc.Scan() {
			if first {
				ready <- sc.Text()
				first = false
			}
		}
		if first {
			ready <- "EOF"
		}
		cmd.Wait()
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			stdin.Close()
			cmd.Process.Kill()
			return fmt.Errorf("中转程序启动失败：%s", line)
		}
	case <-time.After(8 * time.Second):
		stdin.Close()
		cmd.Process.Kill()
		return fmt.Errorf("中转程序启动超时")
	}
	lane.relayCmd, lane.relayStdin, lane.relayPort = cmd, stdin, relayPort
	lane.relayOn.Store(true)
	return nil
}

func (a *App) stopRelay(serial string, lane *Lane) {
	if lane.relayCmd == nil {
		return
	}
	lane.relayOn.Store(false)
	lane.relayStdin.Close()
	lane.relayCmd.Process.Kill()
	a.runAdb(serial, "shell", "pkill", "-f", relayRemotePath+" -listen 127.0.0.1:"+strconv.Itoa(lane.port))
	a.runAdb(serial, "reverse", "--remove", fmt.Sprintf("tcp:%d", lane.relayPort))
	lane.relayCmd = nil
}

// ---- UID → 包名 ----

type pkgCache struct {
	mu       sync.Mutex
	names    map[int][]string
	loadedAt time.Time
}

var systemUIDs = map[int]string{0: "root", 1000: "system", 1001: "phone", 1010: "wifi", 1013: "media", 1051: "nfc", 2000: "shell"}

// packagesOf 返回 uid 对应的包名（共享 UID 时可能有多个）；未识别返回 nil
func (a *App) packagesOf(serial string, uid int) []string {
	if uid < 0 {
		return nil
	}
	appID := uid % 100000 // 多用户：uid = userId*100000 + appId
	a.pkgMu.Lock()
	c := a.pkgs[serial]
	if c == nil {
		c = &pkgCache{names: map[int][]string{}}
		a.pkgs[serial] = c
	}
	a.pkgMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if names, ok := c.names[appID]; ok {
		return names
	}
	if time.Since(c.loadedAt) > 10*time.Second {
		c.loadedAt = time.Now()
		if out, err := a.runAdb(serial, "shell", "pm", "list", "packages", "-U"); err == nil {
			c.names = parsePackages(strings.NewReader(out))
		}
		if names, ok := c.names[appID]; ok {
			return names
		}
	}
	if name, ok := systemUIDs[appID]; ok {
		return []string{name}
	}
	return []string{fmt.Sprintf("uid:%d", uid)}
}

// parsePackages 解析 `pm list packages -U` 的输出：package:com.foo uid:10233[,1010233]
func parsePackages(r io.Reader) map[int][]string {
	m := map[int][]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.HasPrefix(f[0], "package:") {
			continue
		}
		pkg := strings.TrimPrefix(f[0], "package:")
		for _, kv := range f[1:] {
			if !strings.HasPrefix(kv, "uid:") {
				continue
			}
			for _, s := range strings.Split(strings.TrimPrefix(kv, "uid:"), ",") {
				uid, err := strconv.Atoi(s)
				if err != nil {
					continue
				}
				id := uid % 100000 // 多用户下同一 App 的多个 UID 对应同一个 appId，去重
				if !slices.Contains(m[id], pkg) {
					m[id] = append(m[id], pkg)
				}
			}
		}
	}
	for k := range m {
		sort.Strings(m[k])
	}
	return m
}

// appLabel 显示用的 App 名：第一个包名，共享 UID 时附带数量
func appLabel(pkgs []string) string {
	switch len(pkgs) {
	case 0:
		return ""
	case 1:
		return pkgs[0]
	default:
		return fmt.Sprintf("%s (+%d)", pkgs[0], len(pkgs)-1)
	}
}

func relayAvailable() bool {
	entries, err := relayFS.ReadDir("relaybin")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(filepath.Base(e.Name()), "wnrelay-") {
			return true
		}
	}
	return false
}
