package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var requestLogFilters = []string{"JschatLog:D", "OkHttp:D", "*:S"}

var logcatLineRe = regexp.MustCompile(`^\S+\s+\S+\s+(\d+)\s+\d+\s+[VDIWEF]\s+([^:]+?)\s*:\s?(.*)$`)
var requestMethodRe = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(https?://\S+)`)
var requestURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)

type requestLog struct {
	pid, method, url, origin string
}

// 只提取已知的请求地址日志；Domain 的最终 URL 也可能在响应后打印，不能据此统计发网次数。
func parseRequestLog(line string) (requestLog, bool) {
	parts := logcatLineRe.FindStringSubmatch(line)
	if parts == nil {
		return requestLog{}, false
	}
	tag, msg := strings.TrimSpace(parts[2]), parts[3]
	origin := ""
	if tag == "JschatLog" {
		switch {
		case strings.HasPrefix(msg, "Domain --> 最终请求URL:"), strings.HasPrefix(msg, "Domain --> 最终请求URL："), strings.HasPrefix(msg, "Domain --> tryAllDomains newUrl:"):
			origin = "domain"
		case strings.HasPrefix(msg, "JalsatHttp_Url --> "), strings.HasPrefix(msg, "JschatHttp_Url --> "):
			origin = "api"
		}
		if origin != "" {
			if raw := requestURLRe.FindString(msg); raw != "" {
				return requestLog{pid: parts[1], url: raw, origin: origin}, true
			}
		}
	}
	if tag == "OkHttp" && strings.HasPrefix(msg, "--> ") {
		if m := requestMethodRe.FindStringSubmatch(msg); m != nil {
			return requestLog{pid: parts[1], method: m[1], url: m[2], origin: "okhttp"}, true
		}
	}
	return requestLog{}, false
}

func (l *Lane) scanRequestLogs(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 256*1024)
	processes := map[string]string{}
	recentDomains := map[string]time.Time{}
	var refreshed time.Time
	for sc.Scan() {
		if l.ctx.Err() != nil {
			return l.ctx.Err()
		}
		req, ok := parseRequestLog(sc.Text())
		if !ok {
			continue
		}
		// Domain 后紧接的 API 地址通常是同一次调用，合并这对日志；同来源的重复请求仍保留。
		now := time.Now()
		key := req.pid + "\x00" + req.url
		if req.origin == "api" {
			if at, ok := recentDomains[key]; ok && now.Sub(at) < time.Second {
				delete(recentDomains, key)
				continue
			}
		}
		if req.origin == "domain" {
			if len(recentDomains) >= 512 {
				clear(recentDomains)
			}
			recentDomains[key] = now
		}
		// PID 会随 App 重启改变；只在出现请求时刷新，不持续轮询手机进程。
		if time.Since(refreshed) > 10*time.Second || (processes[req.pid] == "" && time.Since(refreshed) > time.Second) {
			processes = l.requestProcesses()
			refreshed = time.Now()
		}
		app := processes[req.pid]
		if app == "" {
			app = "pid:" + req.pid
		}
		l.requestEvent("App 日志", app, req.method, req.url, "")
	}
	return sc.Err()
}

func (l *Lane) requestProcesses() map[string]string {
	return l.app.logProcesses(l.ctx, l.serial)
}

func (a *App) logProcesses(parent context.Context, serial string) map[string]string {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.adb, "-s", serial, "shell", "ps", "-A", "-o", "PID,NAME")
	hideWindow(cmd)
	out, _ := cmd.Output()
	names := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if _, err := strconv.Atoi(f[0]); err != nil {
			continue
		}
		pkg, _, _ := strings.Cut(f[1], ":")
		if validPackageName(pkg) {
			names[f[0]] = f[1]
		}
	}
	return names
}

// 进程归属 Lane 的 context：关闭弱网、USB 断开或退出时自动结束，不清空手机 logcat。
func (l *Lane) watchRequestLogs() {
	if l.app.adb == "" {
		return
	}
	for l.ctx.Err() == nil {
		if err := l.readRequestLogs(); err != nil && l.ctx.Err() == nil {
			l.event(fmt.Sprintf("App 请求日志采集已中断（%v），5 秒后重试；代理流量不受影响", err))
		}
		select {
		case <-l.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (l *Lane) readRequestLogs() error {
	args := []string{"-s", l.serial, "logcat", "-v", "threadtime", "-T", "1"}
	args = append(args, requestLogFilters...)
	cmd := exec.CommandContext(l.ctx, l.app.adb, args...)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	l.event("已开启 App 请求地址日志补充（JschatLog / OkHttp）；方法未记录时留空，日志不代表下载成功")
	scanErr := l.scanRequestLogs(stdout)
	if scanErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}
