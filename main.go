// 弱网模拟器：HTTP/HTTPS 代理 + 网页控制台，参数可在运行中动态调整，支持同时连接多台 Android 手机。
//
// 原理：每台启用弱网的手机分配一个独立的代理端口（通过 adb reverse 走 USB），并设为手机的系统代理。
// 每台手机一条独立的瓶颈链路（上/下行各一个 FIFO 队列），按带宽串行发送，再叠加延迟、抖动和"丢包"
// （模拟 TCP 重传造成的停顿）。手机之间带宽互不抢占，参数统一，便于在同一网络条件下对比不同 App。
//
// 只对使用系统 HTTP 代理的流量生效（绝大多数 App 的 HTTP/HTTPS/WebSocket）。
// UDP（音视频 RTC、QUIC）和自带网络栈、忽略系统代理的 App 不受影响。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

var console *asyncLog

func main() {
	if runCommand(os.Args[1:]) {
		return
	}
	prepareConsole()
	console = newAsyncLog(os.Stderr, 4096)
	log.SetOutput(console)
	log.SetFlags(log.Ltime)
	port := flag.Int("port", 8888, "代理起始端口，每台手机依次占用一个")
	uiPort := flag.Int("ui-port", 8899, "控制台端口")
	preset := flag.String("preset", "weak", "启动档位：unlimited|4g|3g|weak|edge|bad")
	windowKB := flag.Float64("window-kb", 128, "每连接在途数据上限（KB），近似 TCP 拥塞窗口")
	whitelist := flag.String("whitelist", "", "白名单：这些域名永远不走弱网（逗号分隔）")
	blacklist := flag.String("blacklist", "", "黑名单：设置后开启黑名单开关，只有这些域名走弱网（逗号分隔）")
	dnsServer := flag.String("dns", "223.5.5.5", "解析目标域名用的 DNS，留空用系统解析")
	device := flag.Bool("device", false, "只连了一台手机时，启动后直接对它启用弱网")
	serial := flag.String("serial", "", "启动后直接启用这些手机（逗号分隔的序列号）")
	adbPath := flag.String("adb", "", "adb 路径，默认自动查找")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	dataDir := flag.String("data-dir", "", "固定 CA 和 HTTPS 设置目录，默认系统用户配置目录下的 weaknet")
	flag.Parse()

	if findPreset(*preset) == nil {
		fatal("未知档位 %q", *preset)
	}
	cfg := &Config{}
	cfg.p.WindowKB = *windowKB
	cfg.ApplyPreset(*preset)
	cfg.Update(map[string]any{"whitelist": *whitelist, "blacklist": *blacklist, "blacklist_on": *blacklist != ""})

	adb := findAdb(*adbPath)
	app := newApp(cfg, *dnsServer, adb, *port)
	if *dataDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			fatal("读取配置目录失败：%v", err)
		}
		*dataDir = filepath.Join(dir, "weaknet")
	}
	var err error
	app.https, err = openHTTPSManager(*dataDir)
	if err != nil {
		fatal("加载固定 CA 失败：%v", err)
	}
	log.Printf("固定 CA：%s（私钥仅保存在本机）", app.https.dir)
	log.Printf("弱网模拟器 %s", version)
	if adb == "" {
		log.Println("找不到 adb：请安装 Android SDK platform-tools、加入 PATH，或把 adb 放在本程序同目录，也可用 -adb 指定")
	} else {
		log.Println("adb: " + adb)
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *uiPort))
	if err != nil {
		fatal("控制台端口 %d 被占用（是否已经开着一个模拟器？）：%v", *uiPort, err)
	}
	ln.Close()
	go func() {
		if err := app.serveUI(*uiPort); err != nil {
			fatal("控制台启动失败：%v", err)
		}
	}()

	app.refreshDevices()
	var wanted []string
	for _, s := range strings.Split(*serial, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wanted = append(wanted, s)
		}
	}
	if len(wanted) == 0 && *device {
		var online []string
		app.mu.Lock()
		for s, d := range app.devices {
			if d.State == "device" {
				online = append(online, s)
			}
		}
		app.mu.Unlock()
		if len(online) == 1 {
			wanted = online
		} else if len(online) > 1 {
			log.Println("连接了多台手机，不会自动选择，请用 -serial 指定或在控制台里启用")
		}
	}
	for _, s := range wanted {
		app.enable(s)
	}
	app.mu.Lock()
	for s, d := range app.devices {
		tag := ""
		if _, ok := app.lanes[s]; ok {
			tag = "  [弱网]"
		}
		log.Printf("  %s  %s  %s%s", d.Model, s, d.State, tag)
	}
	app.mu.Unlock()
	go app.pollDevices()

	url := fmt.Sprintf("http://127.0.0.1:%d", *uiPort)
	log.Printf("档位 %s，控制台 %s （关闭本窗口或按 Ctrl+C 退出，会自动恢复手机）", findPreset(*preset).Label, url)
	if !*noBrowser {
		time.AfterFunc(300*time.Millisecond, func() { openBrowser(url) })
	}

	// Windows 上点窗口关闭按钮也会以 SIGTERM 送达，有几秒时间完成恢复
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-app.quit:
	}
	log.Println("退出中，恢复已启用弱网的手机...")
	app.shutdown()
	log.Println("已退出")
	console.flush(2 * time.Second)
}

func fatal(format string, args ...any) {
	log.Printf(format, args...)
	if console != nil {
		console.flush(2 * time.Second)
	}
	waitBeforeExit()
	os.Exit(1)
}
