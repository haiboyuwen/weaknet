// wnrelay：运行在 Android 手机上（adb shell 身份，无需 root）的中转程序。
//
// App 通过系统代理连到本程序（127.0.0.1:<listen>），本程序根据对端端口在 /proc/net/tcp(6) 里查出
// 该连接所属的 UID，在转发数据前加一行 "WNUID <uid>\n"，再经 adb reverse 转给电脑上的弱网模拟器，
// 由模拟器把 UID 换成包名，实现按 App 分类和过滤。
//
// stdin 关闭（电脑端 adb shell 断开）时自动退出。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8888", "本地监听地址（手机系统代理指向这里）")
	upstream := flag.String("upstream", "127.0.0.1:18888", "转发地址（adb reverse 到电脑上的模拟器）")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Println("ERR", err)
		os.Exit(1)
	}
	listenPort := ln.Addr().(*net.TCPAddr).Port
	fmt.Println("READY")

	go func() {
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		go handle(c, listenPort, *upstream)
	}
}

// hopBufferBytes 是中转这一段每个 socket 的缓冲大小。
//
// 真实网络里服务器和 App 之间只有 App 自己的接收缓冲：App 读得慢，缓冲一满服务器就得停下来，
// 这是 App 给后台下载限速的基础。经过模拟器时中间多了"模拟器 → adb → 本程序 → App"几段，
// 系统默认会给每段分配几百 KB 到几 MB 的缓冲，App 即使读得很慢，数据也会先以链路全速灌进这些缓冲，
// 模拟器上看到的占用就比真实网络大得多。把缓冲压小，让反压尽快传回模拟器。
// 这几段都在本机或 USB 上，延迟极低，小缓冲不影响吞吐。
const hopBufferBytes = 32 * 1024

// shrinkBuffers 压小一个连接的收发缓冲
func shrinkBuffers(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		t.SetReadBuffer(hopBufferBytes)
		t.SetWriteBuffer(hopBufferBytes)
	}
}

func handle(c net.Conn, listenPort int, upstream string) {
	defer c.Close()
	shrinkBuffers(c)
	peerPort := c.RemoteAddr().(*net.TCPAddr).Port
	uid := -1
	for i := 0; i < 3 && uid < 0; i++ {
		if uid = lookupUID(peerPort, listenPort); uid < 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	up, err := net.DialTimeout("tcp", upstream, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	shrinkBuffers(up)
	if _, err := fmt.Fprintf(up, "WNUID %d\n", uid); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(up, c)
		if t, ok := up.(*net.TCPConn); ok {
			t.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, up)
		if t, ok := c.(*net.TCPConn); ok {
			t.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// lookupUID 在 /proc/net/tcp(6) 中找本地端口为 peerPort、远端端口为 listenPort 的连接（即 App 一侧的 socket），返回其 UID
func lookupUID(peerPort, listenPort int) int {
	for _, path := range []string{"/proc/net/tcp6", "/proc/net/tcp"} {
		if uid := scanUID(path, peerPort, listenPort); uid >= 0 {
			return uid
		}
	}
	return -1
}

func scanUID(path string, peerPort, listenPort int) int {
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()
	return parseUID(f, peerPort, listenPort)
}

func parseUID(r io.Reader, peerPort, listenPort int) int {
	sc := bufio.NewScanner(r)
	sc.Scan() // 表头
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		if hexPort(f[1]) == peerPort && hexPort(f[2]) == listenPort {
			if uid, err := strconv.Atoi(f[7]); err == nil {
				return uid
			}
		}
	}
	return -1
}

func hexPort(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return -1
	}
	p, err := strconv.ParseInt(addr[i+1:], 16, 32)
	if err != nil {
		return -1
	}
	return int(p)
}
