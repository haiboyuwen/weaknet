package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const payloadSize = 200 * 1024

func newTestApp(t *testing.T, preset string) *App {
	t.Helper()
	cfg := &Config{}
	cfg.p.WindowKB = 128
	cfg.ApplyPreset(preset)
	return newApp(cfg, "", "", 0)
}

func startLane(t *testing.T, app *App, name string) *Lane {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	lane := newLane(app, name, name, port)
	if err := lane.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lane.stop)
	return lane
}

func origin(t *testing.T) *httptest.Server {
	t.Helper()
	body := bytes.Repeat([]byte("x"), payloadSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	return srv
}

func fetch(lane *Lane, target string) (int, time.Duration, error) {
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", lane.port))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true}, Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return int(n), time.Since(start), err
}

// 3G 下行 1500 kbps = 187.5 KB/s，200 KB 理论约 1.07 s
func TestRateLimit(t *testing.T) {
	app := newTestApp(t, "3g")
	app.cfg.Update(map[string]any{"loss_pct": 0, "jitter_ms": 0})
	lane := startLane(t, app, "A")
	srv := origin(t)
	n, d, err := fetch(lane, srv.URL)
	if err != nil || n != payloadSize {
		t.Fatalf("n=%d err=%v", n, err)
	}
	want := float64(payloadSize) / (1500 * 1000 / 8)
	t.Logf("200KB @1500kbps: %.2fs (theory %.2fs)", d.Seconds(), want)
	if d.Seconds() < want*0.9 || d.Seconds() > want*1.6 {
		t.Fatalf("rate off: got %.2fs want ~%.2fs", d.Seconds(), want)
	}
}

// 两台手机各自独立的带宽：并发下载耗时应与单独下载相当
func TestLaneIsolation(t *testing.T) {
	app := newTestApp(t, "3g")
	app.cfg.Update(map[string]any{"loss_pct": 0, "jitter_ms": 0})
	a, b := startLane(t, app, "A"), startLane(t, app, "B")
	srv := origin(t)
	_, single, _ := fetch(a, srv.URL)
	var wg sync.WaitGroup
	durs := make([]time.Duration, 2)
	for i, l := range []*Lane{a, b} {
		wg.Add(1)
		go func(i int, l *Lane) { defer wg.Done(); _, durs[i], _ = fetch(l, srv.URL) }(i, l)
	}
	wg.Wait()
	t.Logf("single %.2fs, parallel A %.2fs B %.2fs", single.Seconds(), durs[0].Seconds(), durs[1].Seconds())
	for _, d := range durs {
		if d.Seconds() > single.Seconds()*1.4 {
			t.Fatalf("lanes interfere: %.2fs vs single %.2fs", d.Seconds(), single.Seconds())
		}
	}
}

// 运行中调参数立即生效
func TestLiveUpdate(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := origin(t)
	n1, fast, err1 := fetch(lane, srv.URL)
	t.Logf("first: n=%d err=%v", n1, err1)
	app.cfg.Update(map[string]any{"down_kbps": 800})
	_, slow, _ := fetch(lane, srv.URL)
	t.Logf("unlimited %.2fs, 800kbps %.2fs", fast.Seconds(), slow.Seconds())
	if slow.Seconds() < 1.8 { // 200KB @100KB/s ≈ 2s
		t.Fatalf("update not applied: %.2fs", slow.Seconds())
	}
}

// 断开模式拒绝连接，恢复后正常
func TestOutageReset(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := origin(t)
	app.cfg.Update(map[string]any{"outage": "reset"})
	if _, _, err := fetch(lane, srv.URL); err == nil {
		t.Fatal("expected failure while reset")
	}
	app.cfg.Update(map[string]any{"outage": ""})
	if n, _, err := fetch(lane, srv.URL); err != nil || n != payloadSize {
		t.Fatalf("recover failed n=%d err=%v", n, err)
	}
}

// 客户端中途断开后，连接应很快清理，不再继续从服务器拉数据
func TestClientAbortCleansUp(t *testing.T) {
	app := newTestApp(t, "bad")
	lane := startLane(t, app, "A")
	srv := origin(t)
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lane.port))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET %s/ HTTP/1.1\r\nHost: %s\r\n\r\n", srv.URL, srv.Listener.Addr())
	time.Sleep(1500 * time.Millisecond)
	conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lane.stats().Conns == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("connection not cleaned up, conns=%d", lane.stats().Conns)
}

// 黑名单开关开启：只有名单内的域名走弱网，其余直连不限速，也不受断开影响
func TestBlacklist(t *testing.T) {
	app := newTestApp(t, "bad") // 80 kbps，200KB 至少 20 s
	lane := startLane(t, app, "A")
	srv := origin(t) // 主机名为 127.0.0.1

	app.cfg.Update(map[string]any{"blacklist_on": true, "blacklist": "example.com, foo.bar"})
	n, d, err := fetch(lane, srv.URL)
	t.Logf("not in limit list: n=%d %.2fs err=%v", n, d.Seconds(), err)
	if err != nil || n != payloadSize || d > 500*time.Millisecond {
		t.Fatalf("bypass host should be fast")
	}
	time.Sleep(1200 * time.Millisecond)
	found := false
	for _, h := range lane.stats().Hosts {
		if h.Host == "127.0.0.1" && h.Bypass {
			found = true
		}
	}
	if !found {
		t.Fatalf("bypass host not reported: %+v", lane.stats().Hosts)
	}

	app.cfg.Update(map[string]any{"outage": "reset"})
	if n, _, err := fetch(lane, srv.URL); err != nil || n != payloadSize {
		t.Fatalf("bypass host should ignore outage: n=%d err=%v", n, err)
	}
	app.cfg.Update(map[string]any{"outage": ""})

	app.cfg.Update(map[string]any{"blacklist": []any{"127.0.0.1"}, "down_kbps": 800})
	n, d, err = fetch(lane, srv.URL)
	t.Logf("in limit list @800kbps: n=%d %.2fs err=%v", n, d.Seconds(), err)
	if err != nil || d < 1800*time.Millisecond {
		t.Fatalf("limited host should be shaped")
	}
}

// 切到"断开"时，黑名单外的直连连接不应被断掉
func TestResetKeepsBypassConn(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := origin(t)
	app.cfg.Update(map[string]any{"blacklist_on": true, "blacklist": "example.com"}) // 127.0.0.1 直连

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lane.port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target := srv.Listener.Addr().String()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	if line, _ := br.ReadString('\n'); !strings.Contains(line, "200") {
		t.Fatalf("connect failed: %q", line)
	}
	br.ReadString('\n') // 空行

	if app.cfg.Update(map[string]any{"outage": "reset"}) {
		app.resetShaped()
	}
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("bypass conn broken by reset: %v", err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	if n != payloadSize {
		t.Fatalf("short read %d", n)
	}
}

// 白名单永远不走弱网：黑名单开关关闭时豁免；同时在黑白名单中时白名单优先
func TestWhitelist(t *testing.T) {
	app := newTestApp(t, "bad")
	lane := startLane(t, app, "A")
	srv := origin(t)

	app.cfg.Update(map[string]any{"whitelist": "127.0.0.1"})
	if _, d, err := fetch(lane, srv.URL); err != nil || d > 500*time.Millisecond {
		t.Fatalf("whitelisted host should be fast, %.2fs err=%v", d.Seconds(), err)
	}
	app.cfg.Update(map[string]any{"blacklist_on": true, "blacklist": "127.0.0.1"})
	if _, d, err := fetch(lane, srv.URL); err != nil || d > 500*time.Millisecond {
		t.Fatalf("whitelist should win over blacklist, %.2fs err=%v", d.Seconds(), err)
	}
	// 移出白名单后，黑名单内的域名走弱网
	app.cfg.Update(map[string]any{"whitelist": "", "down_kbps": 800, "loss_pct": 0, "jitter_ms": 0, "delay_ms": 0})
	if _, d, err := fetch(lane, srv.URL); err != nil || d < 1800*time.Millisecond {
		t.Fatalf("blacklisted host should be shaped, %.2fs err=%v", d.Seconds(), err)
	}
	// 开关开启但黑名单不含该域名：直连
	app.cfg.Update(map[string]any{"blacklist": "example.com"})
	if _, d, err := fetch(lane, srv.URL); err != nil || d > 500*time.Millisecond {
		t.Fatalf("host outside blacklist should be fast, %.2fs err=%v", d.Seconds(), err)
	}
	// 关闭开关：所有非白名单域名走弱网
	app.cfg.Update(map[string]any{"blacklist_on": false})
	if _, d, err := fetch(lane, srv.URL); err != nil || d < 1800*time.Millisecond {
		t.Fatalf("switch off should shape all, %.2fs err=%v", d.Seconds(), err)
	}
}

func TestParsePackages(t *testing.T) {
	out := "package:com.google.android.gms uid:10233\npackage:com.google.android.gsf uid:10233\npackage:com.yalla.yallajalsat uid:10685,1010685\n"
	m := parsePackages(strings.NewReader(out))
	if got := m[10685]; len(got) != 1 || got[0] != "com.yalla.yallajalsat" {
		t.Fatalf("10685 -> %v", got)
	}
	if got := appLabel(m[10233]); got != "com.google.android.gms (+1)" {
		t.Fatalf("shared uid label %q", got)
	}
}

// 经中转程序过来的连接（带 WNUID 头）按 App 统计，并且黑名单可以按包名生效
func TestRelayHeaderByPackage(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := origin(t)
	// 预置包名缓存，避免调用 adb
	app.pkgs[lane.serial] = &pkgCache{names: map[int][]string{10685: {"com.yalla.yallajalsat"}, 10233: {"com.google.android.gms"}}, loadedAt: time.Now()}

	get := func(uid int) time.Duration {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lane.port))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		target := srv.Listener.Addr().String()
		start := time.Now()
		fmt.Fprintf(conn, "WNUID %d\nGET http://%s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", uid, target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		if n != payloadSize {
			t.Fatalf("short read %d", n)
		}
		return time.Since(start)
	}

	// 黑名单只填我们 App 的包名：它走弱网，GMS 直连
	app.cfg.Update(map[string]any{"down_kbps": 800, "blacklist_on": true, "blacklist": "com.yalla.yallajalsat"})
	ours, gms := get(10685), get(10233)
	t.Logf("ours %.2fs, gms %.2fs", ours.Seconds(), gms.Seconds())
	if ours < 1800*time.Millisecond || gms > 500*time.Millisecond {
		t.Fatalf("package blacklist not applied")
	}
	time.Sleep(1200 * time.Millisecond)
	apps := map[string]bool{}
	for _, h := range lane.stats().Hosts {
		apps[h.App] = h.Bypass
	}
	if b, ok := apps["com.yalla.yallajalsat"]; !ok || b {
		t.Fatalf("our app should be shaped in stats: %+v", lane.stats().Hosts)
	}
	if b, ok := apps["com.google.android.gms"]; !ok || !b {
		t.Fatalf("gms should be bypass in stats: %+v", lane.stats().Hosts)
	}
	// 白名单填包名优先
	app.cfg.Update(map[string]any{"whitelist": "com.yalla.yallajalsat"})
	if d := get(10685); d > 500*time.Millisecond {
		t.Fatalf("whitelisted package should be fast: %.2fs", d.Seconds())
	}
}

// 连接历史：按 App / 域名筛选、字节统计、CSV 导出、App 曲线
func TestConnHistory(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := origin(t)
	app.pkgs[lane.serial] = &pkgCache{names: map[int][]string{10685: {"com.yalla.yallajalsat"}, 10233: {"com.google.android.gms"}}, loadedAt: time.Now()}
	get := func(uid int) {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lane.port))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		target := srv.Listener.Addr().String()
		fmt.Fprintf(conn, "WNUID %d\nGET http://%s/ HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", uid, target, target)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
	}
	get(10685)
	get(10685)
	get(10233)
	time.Sleep(1500 * time.Millisecond) // 等连接结束、采样一次

	rows, sum := app.conns.query(ConnFilter{App: "com.yalla.yallajalsat"}, 0)
	if sum.Count != 2 || len(rows) != 2 || sum.Active != 0 {
		t.Fatalf("filter by app: %+v", sum)
	}
	if sum.Down < 2*payloadSize || rows[0].Status != "ok" || rows[0].Device != "A" {
		t.Fatalf("bytes/status wrong: %+v %+v", sum, rows[0])
	}
	if _, s := app.conns.query(ConnFilter{Host: "127.0.0.1"}, 0); s.Count != 3 {
		t.Fatalf("filter by host: %+v", s)
	}
	if _, s := app.conns.query(ConnFilter{App: "-"}, 0); s.Count != 0 {
		t.Fatalf("unidentified should be 0: %+v", s)
	}
	var buf bytes.Buffer
	app.conns.writeCSV(&buf, ConnFilter{App: "com.google.android.gms"})
	if lines := strings.Count(buf.String(), "\n"); lines != 2 { // 表头 + 1 行
		t.Fatalf("csv lines %d: %s", lines, buf.String())
	}
	apps := app.conns.apps()
	if len(apps) != 2 || apps[0].App != "com.yalla.yallajalsat" {
		t.Fatalf("apps summary: %+v", apps)
	}
	st := lane.statsFor("com.yalla.yallajalsat")
	if len(st.AppHistory) != len(st.History) || len(st.History) == 0 {
		t.Fatalf("app history misaligned: %d vs %d", len(st.AppHistory), len(st.History))
	}
}

func TestValidPackageName(t *testing.T) {
	for _, ok := range []string{"com.yalla.yallajalsat", "com.tencent.mm", "a.b", "com.google.android.gms_1"} {
		if !validPackageName(ok) {
			t.Fatalf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "com", "com.foo;reboot", "com.foo && rm -rf /", "com.foo bar", "-p com.foo", "com..foo", "1com.foo", "com.foo$(id)"} {
		if validPackageName(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}
