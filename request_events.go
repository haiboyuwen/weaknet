package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

type EventView struct {
	Method        string `json:"method,omitempty"`
	URL           string `json:"url,omitempty"`
	StatusCode    int    `json:"status_code,omitempty"`
	ContentType   string `json:"content_type,omitempty"`
	RequestType   string `json:"request_type,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	RequestBytes  *int64 `json:"request_bytes,omitempty"`
	ResponseBytes *int64 `json:"response_bytes,omitempty"`

	RecordedAt int64  `json:"recorded_at"`
	Started    int64  `json:"started,omitempty"`
	Ended      int64  `json:"ended,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Decrypted  bool   `json:"decrypted,omitempty"`
	ID         uint64 `json:"id"`
	Text       string `json:"text"`
	Status     string `json:"status,omitempty"` // active | complete | failed | timeout | unknown
	Scope      string `json:"scope,omitempty"`  // http | tunnel | log
	Detail     string `json:"detail,omitempty"`
	Serial     string `json:"serial,omitempty"`
	App        string `json:"app,omitempty"`
	Host       string `json:"host,omitempty"`
}

// 更新原事件，不增加日志条数；已经离开最近 300 条的事件不再保留。
func (a *App) finishEvent(id uint64, status, detail string) {
	summary, captured := a.inspections.summary(id)
	a.evMu.Lock()
	defer a.evMu.Unlock()
	for i := range a.eventDetails {
		e := &a.eventDetails[i]
		if e.ID == id && e.Status == "active" {
			e.Status, e.Detail = status, detail
			if captured {
				applyRequestSummary(e, summary)
			}
			e.Ended = max(e.Started, time.Now().UnixMilli())
			if e.Started > 0 {
				e.DurationMS = e.Ended - e.Started
			}
			return
		}
	}
}

func (a *App) failEvent(id uint64, err error) {
	status := "failed"
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		status = "timeout"
	}
	a.finishEvent(id, status, err.Error())
}

// 只观察已成功写入客户端的首个明文 HTTP 响应。解析器不改动转发的字节，
// 读完响应后停止观察，后续 keep-alive 数据仍原样通过。
type responseObserver struct {
	w    *io.PipeWriter
	done chan struct{}
	once sync.Once
}

func newResponseObserver(a *App, id uint64, method string) *responseObserver {
	r, w := io.Pipe()
	o := &responseObserver{w: w, done: make(chan struct{})}
	go func() {
		defer close(o.done)
		defer r.Close()
		br := bufio.NewReader(r)
		for {
			resp, err := http.ReadResponse(br, &http.Request{Method: method})
			if err != nil {
				a.finishEvent(id, "failed", "响应不完整或格式错误")
				return
			}
			if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != 101 {
				resp.Body.Close()
				continue
			}
			a.setEventHTTPMetadata(id, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Proto, nil)
			count, copyErr := io.Copy(io.Discard, resp.Body)
			err = copyErr
			a.setEventHTTPMetadata(id, resp.StatusCode, resp.Header.Get("Content-Type"), resp.Proto, &count)
			resp.Body.Close()
			if err != nil {
				a.finishEvent(id, "failed", "响应传输中断")
			} else if resp.StatusCode >= 400 {
				a.finishEvent(id, "failed", fmt.Sprintf("HTTP %d", resp.StatusCode))
			} else {
				a.finishEvent(id, "complete", fmt.Sprintf("HTTP %d · 响应传输完成", resp.StatusCode))
			}
			return
		}
	}()
	return o
}

func (o *responseObserver) feed(data []byte) { _, _ = o.w.Write(data) }
func (o *responseObserver) close() {
	o.once.Do(func() { o.w.Close(); <-o.done })
}

type observedConn struct {
	net.Conn
	observer *responseObserver
}

func (c *observedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.observer.feed(p[:n])
	}
	return n, err
}

func (c *observedConn) Close() error {
	err := c.Conn.Close()
	c.observer.close()
	return err
}

// displayURL 只保留排查所需的地址：不记录用户信息、fragment 或查询参数值。
func displayURL(raw string) (string, bool) {
	if len(raw) > 8192 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.RawQuery != "" {
		params := strings.Split(u.RawQuery, "&")
		for i, p := range params {
			key, _, hasValue := strings.Cut(p, "=")
			if hasValue && key != "" {
				params[i] = key + "=REDACTED"
			} else {
				params[i] = "REDACTED"
			}
		}
		u.RawQuery = strings.Join(params, "&")
	}
	return u.String(), true
}

func requestKind(raw string) string {
	u, err := url.Parse(raw)
	if err == nil {
		switch strings.ToLower(path.Ext(u.Path)) {
		case ".mp4", ".svga", ".gif", ".webp", ".png", ".jpg", ".jpeg", ".avif",
			".zip", ".mp3", ".wav", ".ogg", ".aac", ".webm", ".ttf", ".otf", ".woff", ".woff2":
			return "资源"
		}
	}
	return "HTTP"
}

func (l *Lane) requestEvent(source, app, method, raw, detail string) uint64 {
	address, ok := displayURL(raw)
	if !ok {
		return 0
	}
	if app == "" {
		app = "未识别 App"
	}
	urlAddress := address
	if method != "" {
		address = method + " " + address
	}
	status, scope := "unknown", "log"
	if source == "代理" {
		status, scope = "active", "http"
	}
	u, _ := url.Parse(raw)
	decrypted := source == "代理" && strings.Contains(detail, "HTTPS 解密")
	return l.app.recordEvent(EventView{Method: method, URL: urlAddress, Text: fmt.Sprintf("[%s] [%s][%s] %s · %s%s", l.name, requestKind(raw), source, address, app, detail), Status: status, Scope: scope, Serial: l.serial, App: app, Host: u.Hostname(), Decrypted: decrypted})
}

func (l *Lane) proxyRequestEvent(app, method, target string, shaped bool) uint64 {
	mode := "直连"
	if shaped {
		mode = "弱网"
	}
	if method == "CONNECT" {
		if app == "" {
			app = "未识别 App"
		}
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			host = target
		}
		return l.app.recordEvent(EventView{Method: "CONNECT", URL: "https://" + target, Text: fmt.Sprintf("[%s] [HTTPS 隧道][代理] CONNECT %s · %s · %s（加密路径不可见）", l.name, target, app, mode), Status: "active", Scope: "tunnel", Serial: l.serial, App: app, Host: host})
	}
	return l.requestEvent("代理", app, method, target, " · "+mode)
}

// Only metadata enters the polling response; headers and bodies remain in the
// bounded, on-demand inspection store. Retain completed summaries with events.
func (a *App) setEventHTTPMetadata(id uint64, code int, contentType, protocol string, count *int64) {
	a.evMu.Lock()
	defer a.evMu.Unlock()
	for i := range a.eventDetails {
		e := &a.eventDetails[i]
		if e.ID == id {
			e.StatusCode, e.ContentType, e.Protocol = code, contentType, protocol
			if count != nil {
				n := *count
				e.ResponseBytes = &n
			}
			return
		}
	}
}
