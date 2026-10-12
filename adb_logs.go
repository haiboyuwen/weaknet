package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const adbLogLimit = 2000

var adbLineRE = regexp.MustCompile(`^(\S+\s+\S+)\s+(\d+)\s+(\d+)\s+([VDIWEF])\s+([^:]+?)\s*:\s?(.*)$`)

type ADBLog struct {
	ID      uint64 `json:"id"`
	Serial  string `json:"serial"`
	Device  string `json:"device"`
	Time    string `json:"time"`
	PID     string `json:"pid"`
	TID     string `json:"tid"`
	Level   string `json:"level"`
	Tag     string `json:"tag"`
	App     string `json:"app"`
	Message string `json:"message"`
}
type ADBLogSession struct {
	Serial  string `json:"serial"`
	Device  string `json:"device"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	cancel  context.CancelFunc
	done    chan struct{}
}
type adbLogStore struct {
	mu       sync.Mutex
	seq      uint64
	next     int
	rows     []ADBLog
	sessions map[string]*ADBLogSession
}
type ADBLogFilter struct{ Serial, App, Level, Query string }
type ADBLogSnapshot struct {
	Rows     []ADBLog        `json:"rows"`
	Sessions []ADBLogSession `json:"sessions"`
	Total    int             `json:"total"`
	Limit    int             `json:"limit"`
}

func parseADBLog(line string) (ADBLog, bool) {
	p := adbLineRE.FindStringSubmatch(line)
	if p == nil {
		return ADBLog{}, false
	}
	msg := p[6]
	if len(msg) > 8192 {
		msg = msg[:8192] + " …[截断]"
	}
	return ADBLog{Time: p[1], PID: p[2], TID: p[3], Level: p[4], Tag: strings.TrimSpace(p[5]), Message: msg}, true
}
func (s *adbLogStore) append(row ADBLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	row.ID = s.seq
	if len(s.rows) == adbLogLimit {
		s.rows[s.next] = row
		s.next = (s.next + 1) % adbLogLimit
	} else {
		s.rows = append(s.rows, row)
	}
}
func (s *adbLogStore) snapshot(f ADBLogFilter) ADBLogSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ADBLogSnapshot{Rows: []ADBLog{}, Sessions: []ADBLogSession{}, Total: len(s.rows), Limit: adbLogLimit}
	q := strings.ToLower(f.Query)
	rank := strings.Index("VDIWEF", f.Level)
	for i := len(s.rows) - 1; i >= 0; i-- {
		r := s.rows[(i+s.next)%len(s.rows)]
		if f.Serial != "" && r.Serial != f.Serial {
			continue
		}
		if f.App != "" && r.App != f.App && !strings.HasPrefix(r.App, f.App+":") {
			continue
		}
		if f.Level != "" && strings.Index("VDIWEF", r.Level) < rank {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(r.Tag+" "+r.Message+" "+r.PID+" "+r.App), q) {
			continue
		}
		out.Rows = append(out.Rows, r)
	}
	for _, session := range s.sessions {
		r := *session
		r.cancel = nil
		out.Sessions = append(out.Sessions, r)
	}
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].Serial < out.Sessions[j].Serial })
	return out
}
func (s *adbLogStore) clear(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if serial == "" {
		s.rows = nil
		s.next = 0
		return
	}
	kept := make([]ADBLog, 0, len(s.rows))
	for i := 0; i < len(s.rows); i++ {
		r := s.rows[(i+s.next)%len(s.rows)]
		if r.Serial != serial {
			kept = append(kept, r)
		}
	}
	s.rows = kept
	s.next = 0
}
func (a *App) startADBLogs(serial string) error {
	if a.adb == "" {
		return fmt.Errorf("找不到 adb")
	}
	a.mu.Lock()
	d, ok := a.devices[serial]
	a.mu.Unlock()
	if !ok || d.State != "device" {
		return fmt.Errorf("手机未连接或未授权调试")
	}
	select {
	case <-a.quit:
		return fmt.Errorf("程序正在退出")
	default:
	}
	s := &a.adbLogs
	s.mu.Lock()
	if existing := s.sessions[serial]; existing != nil && existing.Enabled {
		s.mu.Unlock()
		return nil
	}
	if s.sessions == nil {
		s.sessions = map[string]*ADBLogSession{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &ADBLogSession{Serial: serial, Device: d.Model, Enabled: true, Status: "connecting", cancel: cancel, done: make(chan struct{})}
	s.sessions[serial] = session
	s.mu.Unlock()
	go func() {
		select {
		case <-a.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	go a.watchADBLogs(ctx, session)
	return nil
}
func (a *App) stopADBLogs(serial string) {
	s := &a.adbLogs
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[serial]; session != nil {
		session.Enabled = false
		session.Status = "stopped"
		session.Error = ""
		session.cancel()
	}
}
func (a *App) stopAllADBLogs() {
	s := &a.adbLogs
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, session := range s.sessions {
		session.Enabled = false
		session.Status = "stopped"
		session.cancel()
	}
}
func (a *App) logSessionState(session *ADBLogSession, status, problem string) {
	s := &a.adbLogs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[session.Serial] == session && session.Enabled {
		session.Status = status
		session.Error = problem
	}
}
func (a *App) watchADBLogs(ctx context.Context, session *ADBLogSession) {
	defer close(session.done)
	defer func() {
		s := &a.adbLogs
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.sessions[session.Serial] == session {
			session.Enabled = false
			session.Status = "stopped"
		}
	}()
	for ctx.Err() == nil {
		err := a.readADBLogs(ctx, session)
		if ctx.Err() != nil {
			return
		}
		problem := "日志流已结束"
		if err != nil {
			problem = err.Error()
		}
		a.logSessionState(session, "retrying", problem+"；5 秒后重试")
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// 独立于弱网 Lane，USB 中断自动重试；从最新一条开始，不清手机缓冲区。
func (a *App) readADBLogs(ctx context.Context, session *ADBLogSession) error {
	cmd := exec.CommandContext(ctx, a.adb, "-s", session.Serial, "logcat", "-b", "main", "-b", "system", "-b", "crash", "-v", "threadtime", "-T", "1", "*:V")
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &limitedLogWriter{}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	a.logSessionState(session, "running", "")
	scanErr := a.scanADBLogs(ctx, session, stdout)
	if scanErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("%v %s", waitErr, strings.TrimSpace(stderr.text))
	}
	return nil
}

type limitedLogWriter struct{ text string }

func (w *limitedLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	if room := 4096 - len(w.text); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		w.text += string(p)
	}
	return n, nil
}
func (a *App) scanADBLogs(ctx context.Context, session *ADBLogSession, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	processes := map[string]string{}
	var refreshed time.Time
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		row, ok := parseADBLog(scanner.Text())
		if !ok {
			continue
		}
		if time.Since(refreshed) > 5*time.Second || (processes[row.PID] == "" && time.Since(refreshed) > time.Second) {
			processes = a.logProcesses(ctx, session.Serial)
			refreshed = time.Now()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		row.Serial = session.Serial
		row.Device = session.Device
		row.App = processes[row.PID]
		a.adbLogs.append(row)
	}
	return scanner.Err()
}
