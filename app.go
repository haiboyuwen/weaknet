package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type DeviceInfo struct {
	Serial string `json:"serial"`
	Model  string `json:"model"`
	State  string `json:"state"`
	Proxy  string `json:"proxy"`
}

type App struct {
	https     *HTTPSManager
	cfg       *Config
	dnsServer string
	adb       string
	basePort  int

	mu      sync.Mutex
	lanes   map[string]*Lane
	devices map[string]DeviceInfo
	busy    map[string]string
	resume  map[string]*resumeState // USB 断开时开着弱网的手机，重新连接后自动恢复

	dnsMu    sync.Mutex
	dnsCache map[string]dnsEntry

	pkgMu sync.Mutex
	pkgs  map[string]*pkgCache // serial -> UID 包名缓存

	inspections inspectionStore
	conns       ConnStore // 历史连接
	adbLogs     adbLogStore

	evMu         sync.Mutex
	events       []string
	eventSeq     uint64
	eventDetails []EventView

	quit chan struct{}
	once sync.Once
}

type dnsEntry struct {
	ip     string
	expire time.Time
}

// resumeState 记录等待恢复弱网的手机。USB 断开会清掉 adb reverse 和手机端中转程序，
// 但手机上的系统代理还在，指向已失效的端口，不恢复的话整机断网
type resumeState struct {
	seenAt  time.Time // 重新连接上的时间，零值表示还没连上
	nextTry time.Time
}

const (
	resumeRetryInterval = 5 * time.Second
	// 重新连接后一直恢复失败超过这段时间，放弃恢复并清除手机代理，避免断网
	resumeGiveUp = 2 * time.Minute
)

func newApp(cfg *Config, dnsServer, adb string, basePort int) *App {
	return &App{cfg: cfg, dnsServer: dnsServer, adb: adb, basePort: basePort,
		lanes: map[string]*Lane{}, devices: map[string]DeviceInfo{}, busy: map[string]string{},
		resume:   map[string]*resumeState{},
		dnsCache: map[string]dnsEntry{}, pkgs: map[string]*pkgCache{}, quit: make(chan struct{})}
}

func (a *App) requestQuit() { a.once.Do(func() { close(a.quit) }) }

func (a *App) event(msg string) {
	a.addEvent(msg, "", "")
}

func (a *App) addEvent(msg, status, scope string) uint64 {
	return a.recordEvent(EventView{Text: msg, Status: status, Scope: scope})
}

func (a *App) recordEvent(event EventView) uint64 {
	msg := event.Text
	now := time.Now()
	line := now.Format("15:04:05") + " " + msg
	event.RecordedAt = now.UnixMilli()
	if event.Started == 0 && event.Status == "active" && (event.Scope == "http" || event.Scope == "tunnel") {
		event.Started = event.RecordedAt
	}
	a.evMu.Lock()
	a.eventSeq++
	id := a.eventSeq
	event.ID, event.Text = id, line
	a.events = append([]string{line}, a.events...)
	a.eventDetails = append([]EventView{event}, a.eventDetails...)
	if len(a.events) > 300 {
		a.events = a.events[:300]
		a.eventDetails = a.eventDetails[:300]
	}
	a.evMu.Unlock()
	log.Println(msg)
	return id
}

// shaped 该连接是否走弱网：白名单内的永远不走；黑名单开关关闭时其余全部走，开启时只有黑名单内的走。
// 名单项可以是域名（含子域名），也可以是 App 包名。
func (a *App) shaped(host string, pkgs []string) bool {
	p := a.cfg.Get()
	if matchDomain(host, p.Whitelist) || matchPackage(pkgs, p.Whitelist) {
		return false
	}
	return !p.BlacklistOn || matchDomain(host, p.Blacklist) || matchPackage(pkgs, p.Blacklist)
}

func matchPackage(pkgs, list []string) bool {
	for _, p := range pkgs {
		for _, s := range list {
			if p == s {
				return true
			}
		}
	}
	return false
}

// resolve 用指定 DNS 解析（绕开本机代理软件的 fake-ip），失败时退回系统解析
func (a *App) resolve(host string) string {
	if a.dnsServer == "" || net.ParseIP(host) != nil {
		return host
	}
	a.dnsMu.Lock()
	if e, ok := a.dnsCache[host]; ok && time.Now().Before(e.expire) {
		a.dnsMu.Unlock()
		return e.ip
	}
	a.dnsMu.Unlock()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: 2 * time.Second}
		return d.DialContext(ctx, "udp", net.JoinHostPort(a.dnsServer, "53"))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := r.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return host
	}
	ip := ips[0].String()
	a.dnsMu.Lock()
	a.dnsCache[host] = dnsEntry{ip, time.Now().Add(5 * time.Minute)}
	a.dnsMu.Unlock()
	return ip
}

// ---- adb ----

func (a *App) runAdb(serial string, args ...string) (string, error) {
	if a.adb == "" {
		return "", fmt.Errorf("adb not found")
	}
	if serial != "" {
		args = append([]string{"-s", serial}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.adb, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (a *App) refreshDevices() {
	if a.adb == "" {
		return
	}
	out, err := a.runAdb("", "devices", "-l")
	if err != nil {
		return
	}
	found := map[string]DeviceInfo{}
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for _, line := range lines {
		f := strings.Fields(line)
		if len(f) < 2 || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		d := DeviceInfo{Serial: f[0], State: f[1], Model: f[0]}
		for _, kv := range f[2:] {
			if strings.HasPrefix(kv, "model:") {
				d.Model = strings.ReplaceAll(strings.TrimPrefix(kv, "model:"), "_", " ")
			}
		}
		if d.State == "device" {
			if p, err := a.runAdb(d.Serial, "shell", "settings", "get", "global", "http_proxy"); err == nil {
				d.Proxy = p
			}
		}
		found[d.Serial] = d
	}
	a.mu.Lock()
	for serial, lane := range a.lanes {
		if _, ok := found[serial]; !ok {
			delete(a.lanes, serial)
			lane.stop()
			a.resume[serial] = &resumeState{}
			go a.event(fmt.Sprintf("[%s] 手机已断开 USB，暂停弱网通道，重新连接后自动恢复", lane.name))
		}
	}
	a.devices = found
	toResume := a.dueResumes(found)
	a.mu.Unlock()
	for _, serial := range toResume {
		a.runBusy(serial, "恢复弱网", func() { a.tryResume(serial) })
	}
}

// dueResumes 返回已重新连接、到了重试时间的手机，调用方需持有 a.mu
func (a *App) dueResumes(found map[string]DeviceInfo) []string {
	now := time.Now()
	var due []string
	for serial, st := range a.resume {
		d, ok := found[serial]
		if !ok || d.State != "device" {
			st.seenAt = time.Time{}
			continue
		}
		if st.seenAt.IsZero() {
			st.seenAt = now
		}
		if a.busy[serial] != "" || now.Before(st.nextTry) {
			continue
		}
		st.nextTry = now.Add(resumeRetryInterval)
		due = append(due, serial)
	}
	return due
}

// tryResume 重新启用弱网；刚重新连接时 adb 可能还不可用，失败后由 refreshDevices 按间隔重试，
// 超过 resumeGiveUp 仍失败则清除手机代理
func (a *App) tryResume(serial string) {
	a.enable(serial)
	a.mu.Lock()
	_, enabled := a.lanes[serial]
	st := a.resume[serial]
	giveUp := !enabled && st != nil && time.Since(st.seenAt) > resumeGiveUp
	if enabled || giveUp {
		delete(a.resume, serial)
	}
	name := serial
	if d, ok := a.devices[serial]; ok {
		name = d.Model
	}
	a.mu.Unlock()
	switch {
	case enabled:
		a.event(fmt.Sprintf("[%s] 手机重新连接，已自动恢复弱网", name))
	case giveUp:
		a.runAdb(serial, "shell", "settings", "put", "global", "http_proxy", ":0")
		a.runAdb(serial, "shell", "settings", "delete", "global", "http_proxy")
		a.event(fmt.Sprintf("[%s] 重新连接后恢复弱网失败，已清除手机代理，请手动重新启用", name))
	}
}

func (a *App) pollDevices() {
	for {
		a.refreshDevices()
		select {
		case <-a.quit:
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *App) nextPort() int {
	used := map[int]bool{}
	for _, l := range a.lanes {
		used[l.port] = true
	}
	p := a.basePort
	for used[p] {
		p++
	}
	return p
}

func (a *App) enable(serial string) {
	a.mu.Lock()
	if _, ok := a.lanes[serial]; ok {
		a.mu.Unlock()
		return
	}
	d, ok := a.devices[serial]
	if !ok || d.State != "device" {
		a.mu.Unlock()
		a.event(fmt.Sprintf("[%s] 设备不可用（未连接或未授权调试）", serial))
		return
	}
	lane := newLane(a, serial, d.Model, a.nextPort())
	if err := lane.start(); err != nil {
		a.mu.Unlock()
		a.event(fmt.Sprintf("[%s] 端口 %d 启动失败：%v", d.Model, lane.port, err))
		return
	}
	a.lanes[serial] = lane
	a.mu.Unlock()

	port := fmt.Sprint(lane.port)
	a.runAdb(serial, "reverse", "--remove", "tcp:"+port) // 清理残留，避免占用中转程序要监听的端口
	// 优先用手机端中转程序（可按 App 分类），失败时退回直连
	mode := "按 App 分类"
	var out1 string
	var err1 error
	if relayAvailable() {
		if err := a.startRelay(serial, lane); err != nil {
			a.event(fmt.Sprintf("[%s] %v，退回直连模式（无法按 App 分类）", d.Model, err))
		}
	}
	if lane.relayCmd == nil {
		mode = "直连模式"
		out1, err1 = a.runAdb(serial, "reverse", "tcp:"+port, "tcp:"+port)
	}
	out2, err2 := a.runAdb(serial, "shell", "settings", "put", "global", "http_proxy", "127.0.0.1:"+port)
	if err1 != nil || err2 != nil {
		a.mu.Lock()
		delete(a.lanes, serial)
		a.mu.Unlock()
		a.stopRelay(serial, lane)
		lane.stop()
		a.event(fmt.Sprintf("[%s] 启用失败：%s %s", d.Model, out1, out2))
		return
	}
	a.event(fmt.Sprintf("[%s] 已启用弱网，端口 %s，%s", d.Model, port, mode))
	go lane.watchRequestLogs()
}

func (a *App) disable(serial string) {
	a.mu.Lock()
	lane := a.lanes[serial]
	delete(a.lanes, serial)
	delete(a.resume, serial)
	name := serial
	if d, ok := a.devices[serial]; ok {
		name = d.Model
	}
	a.mu.Unlock()
	a.runAdb(serial, "shell", "settings", "put", "global", "http_proxy", ":0")
	a.runAdb(serial, "shell", "settings", "delete", "global", "http_proxy")
	if lane != nil {
		a.stopRelay(serial, lane)
		a.runAdb(serial, "reverse", "--remove", fmt.Sprintf("tcp:%d", lane.port))
		lane.stop()
	}
	a.event(fmt.Sprintf("[%s] 已关闭弱网，系统代理已清除", name))
}

// reconnect 关开 WiFi，让已建立的连接断开后改走代理
func (a *App) reconnect(serial string) {
	_, disableErr := a.runAdb(serial, "shell", "svc", "wifi", "disable")
	time.Sleep(3 * time.Second)
	_, enableErr := a.runAdb(serial, "shell", "svc", "wifi", "enable")
	a.mu.Lock()
	name := a.devices[serial].Model
	a.mu.Unlock()
	if disableErr != nil || enableErr != nil {
		a.event(fmt.Sprintf("[%s] 关开 WiFi 失败：关闭 %v；开启 %v", name, disableErr, enableErr))
		return
	}
	a.event(fmt.Sprintf("[%s] 已关开 WiFi", name))
}

func (a *App) deviceAction(serial, action string) error {
	fn := map[string]func(string){"on": a.enable, "off": a.disable, "reconnect": a.reconnect}[action]
	if fn == nil {
		return fmt.Errorf("不支持的设备操作：%s", action)
	}
	a.mu.Lock()
	d, exists := a.devices[serial]
	a.mu.Unlock()
	if !exists || d.State != "device" {
		return fmt.Errorf("手机不可用，请检查 USB 连接和调试授权")
	}
	if !a.runBusy(serial, action, func() { fn(serial) }) {
		return fmt.Errorf("该手机正在执行其他操作")
	}
	return nil
}

// runBusy 同一台手机同一时间只执行一个操作，执行中在控制台显示
func (a *App) runBusy(serial, label string, fn func()) bool {
	a.mu.Lock()
	if a.busy[serial] != "" {
		a.mu.Unlock()
		return false
	}
	a.busy[serial] = label
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.busy, serial)
			a.mu.Unlock()
			a.refreshDevices()
		}()
		fn()
	}()
	return true
}

var packageNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z0-9_]+)+$`)

// validPackageName 包名会拼进手机上的 shell 命令，只允许合法包名字符，防止命令注入
func validPackageName(pkg string) bool {
	return len(pkg) <= 255 && packageNameRe.MatchString(pkg)
}

// clearAppData 停止并清空 App 的全部数据（相当于全新安装，登录态也会清除），可选清空后冷启动
func (a *App) clearAppData(serial, pkg string, launch bool) error {
	if !validPackageName(pkg) {
		return fmt.Errorf("包名不合法：%q", pkg)
	}
	name := serial
	a.mu.Lock()
	d, exists := a.devices[serial]
	if !exists || d.State != "device" {
		a.mu.Unlock()
		return fmt.Errorf("手机不可用，请检查 USB 连接和调试授权")
	}
	name = d.Model
	a.mu.Unlock()
	ok := a.runBusy(serial, "清空 "+pkg, func() {
		a.runAdb(serial, "shell", "am", "force-stop", pkg)
		out, err := a.runAdb(serial, "shell", "pm", "clear", pkg)
		if err != nil || !strings.Contains(out, "Success") {
			a.event(fmt.Sprintf("[%s] 清空 %s 失败：%s", name, pkg, out))
			return
		}
		a.event(fmt.Sprintf("[%s] 已清空 %s 的全部数据", name, pkg))
		if launch {
			if out, err := a.runAdb(serial, "shell", "monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1"); err != nil {
				a.event(fmt.Sprintf("[%s] 启动 %s 失败：%s", name, pkg, out))
			} else {
				a.event(fmt.Sprintf("[%s] 已冷启动 %s", name, pkg))
			}
		}
	})
	if !ok {
		return fmt.Errorf("该手机正在执行其他操作")
	}
	return nil
}

// resetShaped 切到"断开"时断掉走弱网的现有连接，黑名单外直连的连接不受影响
func (a *App) resetShaped() {
	a.mu.Lock()
	for _, l := range a.lanes {
		l.resetConns(true)
	}
	a.mu.Unlock()
	a.event("断开走弱网的连接")
}

// shutdown 恢复所有由本工具启用了弱网的手机
func (a *App) shutdown() {
	a.stopAllADBLogs()
	a.mu.Lock()
	serials := make([]string, 0, len(a.lanes))
	for s := range a.lanes {
		serials = append(serials, s)
	}
	a.mu.Unlock()
	for _, s := range serials {
		a.disable(s)
	}
}

// ---- 控制台状态 ----

type DeviceView struct {
	DeviceInfo
	Enabled bool   `json:"enabled"`
	Relay   bool   `json:"relay"` // 是否通过中转程序（可按 App 分类）
	Port    int    `json:"port,omitempty"`
	Busy    string `json:"busy"`
	Foreign bool   `json:"foreign"`
}

type LaneView struct {
	Serial string    `json:"serial"`
	Name   string    `json:"name"`
	Port   int       `json:"port"`
	Stats  LaneStats `json:"stats"`
}

type State struct {
	HTTPS        *HTTPSStatus        `json:"https,omitempty"`
	Config       Params              `json:"config"`
	Presets      []map[string]string `json:"presets"`
	Adb          bool                `json:"adb"`
	Devices      []DeviceView        `json:"devices"`
	Lanes        []LaneView          `json:"lanes"`
	Apps         []AppSummary        `json:"apps"`
	Events       []string            `json:"events"`
	EventSeq     uint64              `json:"event_seq"`
	EventDetails []EventView         `json:"event_details"`
}

func (a *App) state(appFilter string) State {
	st := State{Config: a.cfg.Get(), Adb: a.adb != "", Devices: []DeviceView{}, Lanes: []LaneView{}, Apps: a.conns.apps()}
	if a.https != nil {
		v := a.https.status()
		st.HTTPS = &v
	}
	for _, p := range presets {
		st.Presets = append(st.Presets, map[string]string{"key": p.Key, "label": p.Label})
	}
	a.mu.Lock()
	serials := make([]string, 0, len(a.devices))
	for s := range a.devices {
		serials = append(serials, s)
	}
	sort.Strings(serials)
	for _, s := range serials {
		d := a.devices[s]
		v := DeviceView{DeviceInfo: d, Busy: a.busy[s]}
		if l, ok := a.lanes[s]; ok {
			v.Enabled, v.Port, v.Relay = true, l.port, l.relayOn.Load()
		} else {
			v.Foreign = d.Proxy != "" && d.Proxy != "null" && d.Proxy != ":0"
		}
		st.Devices = append(st.Devices, v)
	}
	lanes := make([]*Lane, 0, len(a.lanes))
	for _, l := range a.lanes {
		lanes = append(lanes, l)
	}
	a.mu.Unlock()
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].port < lanes[j].port })
	for _, l := range lanes {
		st.Lanes = append(st.Lanes, LaneView{l.serial, l.name, l.port, l.statsFor(appFilter)})
	}
	a.evMu.Lock()
	st.Events = append([]string{}, a.events...)
	st.EventSeq = a.eventSeq
	st.EventDetails = append([]EventView{}, a.eventDetails...)
	a.evMu.Unlock()
	now := time.Now().UnixMilli()
	for i := range st.EventDetails {
		e := &st.EventDetails[i]
		if e.Status == "active" && e.Started > 0 {
			e.DurationMS = max(int64(0), now-e.Started)
		}
	}
	return st
}

func findAdb(explicit string) string {
	exe := "adb"
	if runtime.GOOS == "windows" {
		exe = "adb.exe"
	}
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	// 和本程序放在同一目录的 adb
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), exe),
			filepath.Join(filepath.Dir(self), "platform-tools", exe))
	}
	if p, err := exec.LookPath("adb"); err == nil {
		candidates = append(candidates, p)
	}
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			candidates = append(candidates, filepath.Join(v, "platform-tools", exe))
		}
	}
	home, _ := os.UserHomeDir()
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		candidates = append(candidates, filepath.Join(v, "Android", "Sdk", "platform-tools", exe))
	}
	candidates = append(candidates,
		filepath.Join(home, "AppData", "Local", "Android", "Sdk", "platform-tools", exe),
		filepath.Join(home, "Library", "Android", "sdk", "platform-tools", exe),
		filepath.Join(home, "Developer", "Sdk", "Android", "sdk", "platform-tools", exe),
		filepath.Join(home, "Android", "Sdk", "platform-tools", exe))
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}
