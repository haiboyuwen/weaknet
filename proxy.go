package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Params 所有手机共用的一套弱网参数
type Params struct {
	DownKbps float64 `json:"down_kbps"`
	UpKbps   float64 `json:"up_kbps"`
	DelayMs  float64 `json:"delay_ms"` // 单程延迟，RTT 约为两倍
	JitterMs float64 `json:"jitter_ms"`
	LossPct  float64 `json:"loss_pct"`
	WindowKB float64 `json:"window_kb"`
	Outage   string  `json:"outage"` // "" 正常 | "stall" 卡死 | "reset" 断开
	// 白名单：永远不受弱网影响（优先级最高）
	Whitelist []string `json:"whitelist"`
	// 黑名单开关：关闭时除白名单外所有域名走弱网；开启时只有黑名单内（且不在白名单）的域名走弱网
	BlacklistOn bool     `json:"blacklist_on"`
	Blacklist   []string `json:"blacklist"`
	Preset      string   `json:"preset"`
}

type Preset struct {
	Key, Label                                   string
	DownKbps, UpKbps, DelayMs, JitterMs, LossPct float64
}

var presets = []Preset{
	{"unlimited", "不限速", 0, 0, 0, 0, 0},
	{"4g", "4G", 20000, 5000, 25, 10, 0},
	{"3g", "3G", 1500, 500, 80, 30, 0.5},
	{"weak", "弱网", 400, 200, 200, 50, 1},
	{"edge", "2G/EDGE", 200, 100, 300, 100, 2},
	{"bad", "极弱网", 80, 40, 500, 200, 5},
}

func findPreset(key string) *Preset {
	for i := range presets {
		if presets[i].Key == key {
			return &presets[i]
		}
	}
	return nil
}

type Config struct {
	mu sync.RWMutex
	p  Params
}

func (c *Config) Get() Params {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p := c.p
	p.Whitelist = append([]string{}, c.p.Whitelist...)
	p.Blacklist = append([]string{}, c.p.Blacklist...)
	return p
}

func (c *Config) ApplyPreset(key string) bool {
	pr := findPreset(key)
	if pr == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.p.DownKbps, c.p.UpKbps, c.p.DelayMs, c.p.JitterMs, c.p.LossPct = pr.DownKbps, pr.UpKbps, pr.DelayMs, pr.JitterMs, pr.LossPct
	c.p.Preset = key
	return true
}

// Update 合并部分参数，返回 outage 是否从非 reset 变为 reset
func (c *Config) Update(data map[string]any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	num := func(key string, dst *float64) bool {
		v, ok := data[key]
		if !ok {
			return false
		}
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case int:
			f = float64(x)
		case int64:
			f = float64(x)
		case string:
			fmt.Sscanf(x, "%g", &f)
		default:
			return false
		}
		if f < 0 {
			f = 0
		}
		*dst = f
		return true
	}
	shaped := false
	shaped = num("down_kbps", &c.p.DownKbps) || shaped
	shaped = num("up_kbps", &c.p.UpKbps) || shaped
	shaped = num("delay_ms", &c.p.DelayMs) || shaped
	shaped = num("jitter_ms", &c.p.JitterMs) || shaped
	shaped = num("loss_pct", &c.p.LossPct) || shaped
	num("window_kb", &c.p.WindowKB)
	if shaped {
		c.p.Preset = ""
	}
	becameReset := false
	if v, ok := data["outage"].(string); ok && (v == "" || v == "stall" || v == "reset") {
		becameReset = v == "reset" && c.p.Outage != "reset"
		c.p.Outage = v
	}
	if v, ok := data["whitelist"]; ok {
		c.p.Whitelist = parseList(v)
	}
	if v, ok := data["blacklist"]; ok {
		c.p.Blacklist = parseList(v)
	}
	if v, ok := data["blacklist_on"].(bool); ok {
		c.p.BlacklistOn = v
	}
	return becameReset
}

// parseList 解析逗号分隔的字符串或字符串数组
func parseList(v any) []string {
	var items []string
	switch x := v.(type) {
	case string:
		items = strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '，' })
	case []any:
		for _, it := range x {
			if s, ok := it.(string); ok {
				items = append(items, s)
			}
		}
	case []string:
		items = v.([]string)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// matchDomain 域名等于名单项，或是名单项的子域名
func matchDomain(host string, list []string) bool {
	for _, s := range list {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}

type chunk struct {
	p    *Pipe
	data []byte // nil 表示该方向结束
}

// Link 一条单向瓶颈链路：按带宽串行发送，再交给连接按延迟投递
type Link struct {
	cfg        *Config
	down       bool
	ch         chan chunk
	queued     atomic.Int64
	sent       atomic.Int64
	mu         sync.Mutex
	hostSent   map[string]int64
	hostQueued map[string]int64
}

func newLink(cfg *Config, down bool) *Link {
	return &Link{cfg: cfg, down: down, ch: make(chan chunk, 1<<16),
		hostSent: map[string]int64{}, hostQueued: map[string]int64{}}
}

func (l *Link) rate(p Params) float64 {
	kbps := p.UpKbps
	if l.down {
		kbps = p.DownKbps
	}
	return kbps * 1000 / 8
}

func (l *Link) addQueued(host string, n int) {
	l.queued.Add(int64(n))
	l.mu.Lock()
	l.hostQueued[host] += int64(n)
	l.mu.Unlock()
}

func (l *Link) run(ctx context.Context) {
	// 按绝对时间累计发送进度：Windows 的 sleep 精度约 15ms，逐块 sleep 会严重偏离设定带宽
	var next time.Time
	for {
		var c chunk
		select {
		case <-ctx.Done():
			return
		case c = <-l.ch:
		}
		for l.cfg.Get().Outage == "stall" {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		n := len(c.data)
		if n > 0 {
			if r := l.rate(l.cfg.Get()); r > 0 {
				now := time.Now()
				if next.Before(now) {
					next = now
				}
				next = next.Add(time.Duration(float64(n) / r * float64(time.Second)))
				if d := time.Until(next); d > 2*time.Millisecond {
					time.Sleep(d)
				}
			} else {
				next = time.Time{}
			}
		}
		l.queued.Add(-int64(n))
		l.sent.Add(int64(n))
		l.mu.Lock()
		l.hostSent[c.p.host] += int64(n)
		l.hostQueued[c.p.host] -= int64(n)
		l.mu.Unlock()
		c.p.schedule(c.data)
	}
}

type delivery struct {
	at   time.Time
	data []byte
}

// Pipe 一个连接的一个方向：src -> link -> dst，单独的投递协程保证同一连接内按序投递
type Pipe struct {
	lane     *Lane
	link     *Link
	dst      net.Conn
	host     string
	onDead   func()
	onError  func(error)
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	total    int64
	closed   bool
	lastAt   time.Time // 仅由 link 协程访问
	dq       chan delivery
	counter  *atomic.Int64 // 已投递字节数（写入连接记录）
}

func newPipe(lane *Lane, link *Link, dst net.Conn, host string, onDead func(), counter *atomic.Int64, onError func(error)) *Pipe {
	p := &Pipe{lane: lane, link: link, dst: dst, host: host, onDead: onDead, onError: onError, counter: counter, dq: make(chan delivery, 1<<14)}
	p.cond = sync.NewCond(&p.mu)
	go p.deliverLoop()
	return p
}

func (p *Pipe) window() int {
	w := int(p.lane.cfg.Get().WindowKB * 1024)
	if w < 4096 {
		w = 4096
	}
	return w
}

func (p *Pipe) enqueue(data []byte) {
	p.mu.Lock()
	p.inflight += len(data)
	p.total += int64(len(data))
	p.mu.Unlock()
	p.link.addQueued(p.host, len(data))
	p.link.ch <- chunk{p, data}
}

func (p *Pipe) pump(src io.Reader) {
	buf := make([]byte, 4096)
	for {
		p.mu.Lock()
		for p.inflight >= p.window() && !p.closed {
			p.cond.Wait()
		}
		closed := p.closed
		p.mu.Unlock()
		if closed {
			break
		}
		n, err := src.Read(buf)
		if n > 0 {
			p.enqueue(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				p.onError(err)
			}
			break
		}
	}
	p.link.ch <- chunk{p, nil}
}

func (p *Pipe) schedule(data []byte) {
	cfg := p.lane.cfg.Get()
	delay := time.Duration(cfg.DelayMs * float64(time.Millisecond))
	if cfg.JitterMs > 0 {
		delay += time.Duration(rand.Float64() * cfg.JitterMs * float64(time.Millisecond))
	}
	if len(data) > 0 && cfg.LossPct > 0 && rand.Float64() < cfg.LossPct/100 {
		// TCP 之上无法真正丢包，这里用一次重传超时的停顿来模拟丢包的效果
		rto := time.Duration(cfg.DelayMs * 4 * float64(time.Millisecond))
		if rto < 200*time.Millisecond {
			rto = 200 * time.Millisecond
		}
		delay += rto
		p.lane.lossEvents.Add(1)
	}
	// 延迟/抖动变化时不能乱序：投递时间严格递增（同时模拟了 TCP 的队头阻塞）
	at := time.Now().Add(delay)
	if !at.After(p.lastAt) {
		at = p.lastAt.Add(time.Microsecond)
	}
	p.lastAt = at
	p.dq <- delivery{at, data}
}

func (p *Pipe) deliverLoop() {
	for d := range p.dq {
		if w := time.Until(d.at); w > 0 {
			time.Sleep(w)
		}
		if d.data == nil {
			p.markClosed()
			p.dst.Close()
			return
		}
		p.mu.Lock()
		p.inflight -= len(d.data)
		closed := p.closed
		p.cond.Broadcast()
		p.mu.Unlock()
		if closed {
			continue
		}
		n, err := p.dst.Write(d.data)
		if p.counter != nil {
			p.counter.Add(int64(n))
		}
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.onError(err)
			}
			// 对端已关闭（如客户端取消下载）：关掉整个连接，让服务器也停止发送
			p.markClosed()
			p.onDead()
		}
	}
}

func (p *Pipe) markClosed() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Lane 一台手机的弱网通道：独立的代理端口和独立的上下行链路
type Lane struct {
	app        *App
	cfg        *Config
	serial     string
	name       string
	port       int
	ln         net.Listener
	up, down   *Link
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	active     map[*connPair]struct{}
	lossEvents atomic.Int64
	statMu     sync.Mutex
	history    [][4]float64
	hostRates  map[string]float64
	// 不在黑名单、直连的流量统计
	bypassMu    sync.Mutex
	bypassSent  map[string]int64
	bypassRates map[string]float64
	appHist     map[string][]float64 // App -> 每秒下行 KB/s，与 history 对齐
	// 手机端中转程序（按 App 分类用），为 nil 时是直连模式
	relayCmd   *exec.Cmd
	relayStdin io.WriteCloser
	relayPort  int
	relayOn    atomic.Bool
}

type connPair struct {
	tunnel           bool
	client, upstream net.Conn
	pipes            []*Pipe
	record           *ConnRecord
}

func newLane(app *App, serial, name string, port int) *Lane {
	ctx, cancel := context.WithCancel(context.Background())
	return &Lane{app: app, cfg: app.cfg, serial: serial, name: name, port: port,
		up: newLink(app.cfg, false), down: newLink(app.cfg, true), ctx: ctx, cancel: cancel,
		active: map[*connPair]struct{}{}, hostRates: map[string]float64{},
		bypassSent: map[string]int64{}, bypassRates: map[string]float64{}, appHist: map[string][]float64{}}
}

func (l *Lane) start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", l.port))
	if err != nil {
		return err
	}
	l.ln = ln
	go l.up.run(l.ctx)
	go l.down.run(l.ctx)
	go l.sample()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go l.handle(c)
		}
	}()
	return nil
}

func (l *Lane) stop() {
	if l.relayCmd != nil { // 手机已拔掉等情况下，结束本地的 adb shell 进程
		l.relayStdin.Close()
		l.relayCmd.Process.Kill()
	}
	l.cancel()
	if l.ln != nil {
		l.ln.Close()
	}
	l.resetConns(false)
}

func (l *Lane) event(msg string) { l.app.event(fmt.Sprintf("[%s] %s", l.name, msg)) }

// resetConns 断开连接；shapedOnly 时只断开走弱网（黑名单内）的连接，直连的不受影响
func (l *Lane) resetConns(shapedOnly bool) {
	l.resetMatching(func(c *connPair) bool { return !shapedOnly || c.pipes != nil }, "网络连接已主动断开")
}

func (l *Lane) resetMatching(match func(*connPair) bool, reason string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for c := range l.active {
		if !match(c) {
			continue
		}
		delete(l.active, c)
		count++
		if c.record != nil {
			c.record.finish("failed", reason)
		}
		c.client.Close()
		c.upstream.Close()
		for _, p := range c.pipes {
			p.markClosed()
		}
	}
	return count
}

func readHead(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		sb.WriteString(line)
		if err != nil {
			return "", err
		}
		if line == "\r\n" || line == "\n" {
			return sb.String(), nil
		}
		if sb.Len() > 64*1024 {
			return "", fmt.Errorf("header too large")
		}
	}
}

// clientSendBufferBytes 是模拟器发往手机这一侧 socket 的发送缓冲。
// 压小是为了让 App 读得慢时的反压尽快传回来，见 relay/main.go 里 hopBufferBytes 的说明
const clientSendBufferBytes = 32 * 1024

func (l *Lane) handle(conn net.Conn) {
	if t, ok := conn.(*net.TCPConn); ok {
		t.SetWriteBuffer(clientSendBufferBytes)
	}
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	// 经手机端中转程序过来的连接，开头带一行 "WNUID <uid>"，用来识别发起连接的 App
	var pkgs []string
	if b, err := br.Peek(6); err == nil && string(b) == "WNUID " {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return
		}
		uid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "WNUID ")))
		pkgs = l.app.packagesOf(l.serial, uid)
	}
	app := appLabel(pkgs)
	head, err := readHead(br)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	parts := strings.SplitN(strings.SplitN(head, "\r\n", 2)[0], " ", 3)
	if len(parts) < 3 {
		conn.Close()
		return
	}
	method, target := parts[0], parts[1]

	var host, port string
	var initial []byte
	if method == "CONNECT" {
		host, port, err = net.SplitHostPort(target)
		if err != nil {
			conn.Close()
			return
		}
	} else {
		u, err := url.Parse(target)
		if err != nil || u.Hostname() == "" {
			conn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
			conn.Close()
			return
		}
		host, port = u.Hostname(), u.Port()
		if port == "" {
			port = "80"
		}
		initial = []byte(strings.Replace(head, target, u.RequestURI(), 1))
	}

	shaped := l.app.shaped(host, pkgs)
	if method == "CONNECT" && l.app.https.enabledFor(host) {
		l.handleHTTPS(conn, br, host, port, app, shaped)
		return
	}
	eventID := l.proxyRequestEvent(app, method, target, shaped)
	key := statKey(app, host)
	rec := l.app.conns.open(l, app, host, port, shaped)
	rec.onFinish = func(status, message string) {
		if status == "ok" {
			if method == "CONNECT" {
				l.app.finishEvent(eventID, "complete", "隧道连接已结束，无法判定加密请求结果")
			}
		} else {
			l.app.finishEvent(eventID, "failed", message)
		}
	}
	onError := func(err error) {
		l.app.failEvent(eventID, err)
		rec.finish("failed", err.Error())
	}
	if shaped && l.cfg.Get().Outage == "reset" {
		rec.finish("rejected", "断开模式")
		conn.Close()
		return
	}

	addr := l.app.resolve(host)
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(addr, port), 15*time.Second)
	if err != nil {
		onError(err)
		l.event(fmt.Sprintf("连接失败 %s:%s %v", host, port, err))
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		conn.Close()
		return
	}
	defer rec.finish("ok", "")
	if method != "CONNECT" {
		observer := newResponseObserver(l.app, eventID, method)
		defer observer.close()
		conn = &observedConn{Conn: conn, observer: observer}
	}

	if !shaped {
		l.bypass(conn, br, upstream, key, method, initial, rec, onError)
		return
	}

	l.relayShaped(conn, br, upstream, key, method, initial, rec, onError, host, app)
}

// Both ordinary tunnels and decrypted HTTPS shape the encrypted client stream.
func (l *Lane) relayShaped(conn net.Conn, br *bufio.Reader, upstream net.Conn, key, method string, initial []byte, rec *ConnRecord, onError func(error), host, app string) {
	var once sync.Once
	onDead := func() {
		once.Do(func() {
			conn.Close()
			upstream.Close()
		})
	}
	upPipe := newPipe(l, l.up, upstream, key, onDead, &rec.up, onError)
	downPipe := newPipe(l, l.down, conn, key, onDead, &rec.down, onError)
	pair := &connPair{tunnel: method == "CONNECT", client: conn, upstream: upstream, pipes: []*Pipe{upPipe, downPipe}, record: rec}
	l.mu.Lock()
	l.active[pair] = struct{}{}
	l.mu.Unlock()
	start := time.Now()
	// 近似 TCP 握手的一个 RTT
	time.Sleep(time.Duration(l.cfg.Get().DelayMs * 2 * float64(time.Millisecond)))
	if method == "CONNECT" {
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			onError(err)
			onDead()
		}
	} else {
		upPipe.enqueue(initial)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		upPipe.pump(br) // br 里可能已缓存了请求头之后的数据
	}()
	downPipe.pump(upstream)
	wg.Wait()

	l.mu.Lock()
	delete(l.active, pair)
	l.mu.Unlock()
	if total := downPipe.total; total > 64*1024 {
		who := host
		if app != "" {
			who = app + " " + host
		}
		l.event(fmt.Sprintf("%s 下行 %d KB，%.1f s", who, total/1024, time.Since(start).Seconds()))
	}
}

// bypass 不走弱网的域名（白名单内，或黑名单开启时不在黑名单内）：直连转发，只统计流量
func (l *Lane) bypass(conn net.Conn, br *bufio.Reader, upstream net.Conn, key, method string, initial []byte, rec *ConnRecord, onError func(error)) {
	pair := &connPair{tunnel: method == "CONNECT", client: conn, upstream: upstream, record: rec}
	l.mu.Lock()
	l.active[pair] = struct{}{}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.active, pair)
		l.mu.Unlock()
	}()
	if method == "CONNECT" {
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			onError(err)
			conn.Close()
			upstream.Close()
			return
		}
	} else if n, err := upstream.Write(initial); err != nil {
		onError(err)
		conn.Close()
		upstream.Close()
		return
	} else {
		rec.up.Add(int64(n))
	}
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(countingWriter{upstream, func(n int) { rec.up.Add(int64(n)) }}, br)
		done <- err
	}()
	go func() {
		_, err := io.Copy(countingWriter{conn, func(n int) { l.addBypass(key, n); rec.down.Add(int64(n)) }}, upstream)
		done <- err
	}()
	if err := <-done; err != nil {
		onError(err)
	}
	conn.Close()
	upstream.Close()
	<-done
}

type countingWriter struct {
	w     io.Writer
	count func(int)
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.count(n)
	return n, err
}

func (l *Lane) addBypass(host string, n int) {
	l.bypassMu.Lock()
	l.bypassSent[host] += int64(n)
	l.bypassMu.Unlock()
}

func (l *Lane) sample() {
	var lastUp, lastDown int64
	lastHost := map[string]int64{}
	windows := map[string][]int64{}
	lastBypass := map[string]int64{}
	bwindows := map[string][]int64{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
		up, down := l.up.sent.Load(), l.down.sent.Load()
		upRate, downRate := float64(up-lastUp)/1024, float64(down-lastDown)/1024
		lastUp, lastDown = up, down
		q := 0.0
		if r := l.down.rate(l.cfg.Get()); r > 0 && l.down.queued.Load() > 0 {
			q = float64(l.down.queued.Load()) / r
		}
		l.down.mu.Lock()
		cur := make(map[string]int64, len(l.down.hostSent))
		for h, v := range l.down.hostSent {
			cur[h] = v
		}
		l.down.mu.Unlock()
		rates := map[string]float64{}
		for h, v := range cur {
			w := append(windows[h], v-lastHost[h])
			if len(w) > 5 {
				w = w[len(w)-5:]
			}
			windows[h] = w
			var sum int64
			for _, x := range w {
				sum += x
			}
			rates[h] = float64(sum) / float64(len(w)) / 1024
		}
		appDelta := map[string]int64{} // 本秒每个 App 的下行字节（含直连）
		for h, v := range cur {
			a, _ := splitKey(h)
			appDelta[a] += v - lastHost[h]
		}
		lastHost = cur
		l.bypassMu.Lock()
		bcur := make(map[string]int64, len(l.bypassSent))
		for h, v := range l.bypassSent {
			bcur[h] = v
		}
		l.bypassMu.Unlock()
		brates := map[string]float64{}
		for h, v := range bcur {
			w := append(bwindows[h], v-lastBypass[h])
			if len(w) > 5 {
				w = w[len(w)-5:]
			}
			bwindows[h] = w
			var sum int64
			for _, x := range w {
				sum += x
			}
			brates[h] = float64(sum) / float64(len(w)) / 1024
		}
		for h, v := range bcur {
			a, _ := splitKey(h)
			appDelta[a] += v - lastBypass[h]
		}
		lastBypass = bcur
		l.statMu.Lock()
		l.bypassRates = brates
		l.history = append(l.history, [4]float64{float64(time.Now().Unix()), round1(downRate), round1(upRate), round2(q)})
		if len(l.history) > 180 {
			l.history = l.history[len(l.history)-180:]
		}
		l.hostRates = rates
		// 每个 App 的下行曲线，与 history 按下标对齐
		n := len(l.history)
		for a, d := range appDelta {
			if _, ok := l.appHist[a]; !ok && d > 0 {
				l.appHist[a] = make([]float64, n-1)
			}
		}
		for a, series := range l.appHist {
			series = append(series, round1(float64(appDelta[a])/1024))
			if len(series) > n {
				series = series[len(series)-n:]
			}
			l.appHist[a] = series
		}
		l.statMu.Unlock()
	}
}

type HostStat struct {
	App      string  `json:"app"` // 发起连接的 App 包名，未识别为空
	Host     string  `json:"host"`
	KBs      float64 `json:"kbs"`
	QueuedKB int64   `json:"queued_kb"`
	Bypass   bool    `json:"bypass"` // 不走弱网，直连
}

type LaneStats struct {
	Conns       int          `json:"conns"`
	QueueDownKB int64        `json:"queue_down_kb"`
	QueueDownS  float64      `json:"queue_down_s"`
	LossEvents  int64        `json:"loss_events"`
	History     [][4]float64 `json:"history"`
	Hosts       []HostStat   `json:"hosts"`
	// 指定 App 时该 App 的下行曲线：[时间戳, KB/s]
	AppHistory [][2]float64 `json:"app_history,omitempty"`
}

func (l *Lane) stats() LaneStats { return l.statsFor("") }

// statsFor 返回通道统计；app 非空时附带该 App 的下行曲线
func (l *Lane) statsFor(app string) LaneStats {
	l.mu.Lock()
	conns := len(l.active)
	l.mu.Unlock()
	queued := l.down.queued.Load()
	if queued < 0 {
		queued = 0
	}
	s := LaneStats{Conns: conns, QueueDownKB: queued / 1024, LossEvents: l.lossEvents.Load(), Hosts: []HostStat{}}
	if r := l.down.rate(l.cfg.Get()); r > 0 {
		s.QueueDownS = round2(float64(queued) / r)
	}
	l.down.mu.Lock()
	hostQueued := make(map[string]int64, len(l.down.hostQueued))
	for h, v := range l.down.hostQueued {
		hostQueued[h] = v
	}
	l.down.mu.Unlock()
	l.statMu.Lock()
	s.History = append([][4]float64{}, l.history...)
	if app != "" {
		if app == "-" { // 未识别 App
			app = ""
		}
		series := l.appHist[app]
		s.AppHistory = [][2]float64{}
		for i := range s.History {
			v := 0.0
			if j := i - (len(s.History) - len(series)); j >= 0 {
				v = series[j]
			}
			s.AppHistory = append(s.AppHistory, [2]float64{s.History[i][0], v})
		}
	}
	for h, r := range l.hostRates {
		q := hostQueued[h]
		if q < 0 {
			q = 0
		}
		if r > 0.05 || q > 1024 {
			app, host := splitKey(h)
			s.Hosts = append(s.Hosts, HostStat{app, host, round1(r), q / 1024, false})
		}
	}
	for h, r := range l.bypassRates {
		if r > 0.05 {
			app, host := splitKey(h)
			s.Hosts = append(s.Hosts, HostStat{app, host, round1(r), 0, true})
		}
	}
	l.statMu.Unlock()
	sort.Slice(s.Hosts, func(i, j int) bool { return s.Hosts[i].KBs > s.Hosts[j].KBs })
	if len(s.Hosts) > 60 {
		s.Hosts = s.Hosts[:60]
	}
	return s
}

// statKey 流量统计按 "App<TAB>域名" 分组
func statKey(app, host string) string { return app + "\t" + host }

func splitKey(key string) (app, host string) {
	if i := strings.IndexByte(key, '\t'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
