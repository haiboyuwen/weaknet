package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func waitEventStatus(t *testing.T, app *App, id uint64, status string) EventView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range app.state("").EventDetails {
			if e.ID == id && e.Status == status {
				return e
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %d did not reach %s: %+v", id, status, app.state("").EventDetails)
	return EventView{}
}

func TestResponseEventLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, method, response, status string }{
		{"length", "GET", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok", "complete"},
		{"chunked", "GET", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n", "complete"},
		{"head", "HEAD", "HTTP/1.1 200 OK\r\nContent-Length: 200\r\n\r\n", "complete"},
		{"interim", "GET", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n", "complete"},
		{"http error", "GET", "HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n", "failed"},
		{"truncated", "GET", "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nok", "failed"},
		{"malformed", "GET", "invalid response\r\n\r\n", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			lane := newLane(app, "test", "phone", 0)
			id := lane.proxyRequestEvent("", tc.method, "http://example.com/file", false)
			waitEventStatus(t, app, id, "active")
			o := newResponseObserver(app, id, tc.method)
			for _, b := range []byte(tc.response) {
				o.feed([]byte{b})
			}
			o.close()
			waitEventStatus(t, app, id, tc.status)
			if len(app.state("").Events) != 1 || app.state("").EventSeq != 1 {
				t.Fatal("生命周期更新不应创建重复事件")
			}
		})
	}
}

func TestRequestTimeoutUnknownAndEviction(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := newLane(app, "test", "phone", 0)
	id := lane.proxyRequestEvent("", "GET", "http://example.com", false)
	app.failEvent(id, os.ErrDeadlineExceeded)
	waitEventStatus(t, app, id, "timeout")
	app.finishEvent(id, "complete", "late update")
	waitEventStatus(t, app, id, "timeout")
	logID := lane.requestEvent("App 日志", "com.example", "GET", "https://example.com", "")
	waitEventStatus(t, app, logID, "unknown")
	for i := 0; i < 301; i++ {
		app.event("tool event")
	}
	app.finishEvent(id, "failed", "evicted")
	st := app.state("")
	if len(st.EventDetails) != 300 || len(st.Events) != 300 {
		t.Fatal("事件列表必须有界")
	}
	st.EventDetails[0].Text = "mutated"
	if app.state("").EventDetails[0].Text == "mutated" {
		t.Fatal("状态快照不应共享事件内存")
	}
}

func TestHTTPEventCompletesBeforeKeepAliveCloses(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprint(bypass), func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			if bypass {
				app.cfg.Update(map[string]any{"whitelist": "127.0.0.1"})
			}
			lane := startLane(t, app, "A")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
			defer srv.Close()
			proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", lane.port))
			tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			resp, err := client.Get(srv.URL + "/keepalive")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(body) != "ok" {
				t.Fatalf("forwarded body = %q, %v", body, err)
			}
			waitEventStatus(t, app, 1, "complete")
			rows, _ := app.conns.query(ConnFilter{}, 100)
			if len(rows) != 1 || rows[0].Status != "active" {
				t.Fatalf("request should finish while connection stays active: %+v", rows)
			}
		})
	}
}

func TestProxyResetAndDialFailureEvents(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			if reset {
				app.cfg.Update(map[string]any{"outage": "reset"})
			}
			lane := startLane(t, app, "A")
			_, _, _ = fetch(lane, "http://127.0.0.1:1/fail")
			waitEventStatus(t, app, 1, "failed")
		})
	}
}

func TestDisplayURL(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		ok        bool
	}{
		{"https://cdn.example/gift.mp4", "https://cdn.example/gift.mp4", true},
		{"http://[::1]:8080/a%20b.svga?token=secret&offset=1&token=other#secret", "http://[::1]:8080/a%20b.svga?token=REDACTED&offset=REDACTED&token=REDACTED", true},
		{"https://name:password@example.com/api?payload=%7B%22secret%22%3A1%7D", "https://example.com/api?payload=REDACTED", true},
		{"https://example.com/api?bare-secret&=secret&key=secret;other=secret", "https://example.com/api?REDACTED&REDACTED&key=REDACTED", true},
		{"https://example.com/a\nb", "", false},
		{"file:///private/data", "", false},
		{"https:///no-host", "", false},
		{strings.Repeat("x", 8193), "", false},
	} {
		t.Run(tc.raw[:min(70, len(tc.raw))], func(t *testing.T) {
			got, ok := displayURL(tc.raw)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("displayURL = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRequestKind(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://cdn.example/video.MP4?token=1", "资源"},
		{"https://cdn.example/a%2Esvga", "资源"},
		{"https://cdn.example/image.webp", "资源"},
		{"https://api.example/room/info?name=file.mp4", "HTTP"},
		{"https://api.example/list.json", "HTTP"},
	} {
		if got := requestKind(tc.raw); got != tc.want {
			t.Errorf("requestKind(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func eventText(app *App) string {
	app.evMu.Lock()
	defer app.evMu.Unlock()
	return strings.Join(app.events, "\n")
}

func TestHTTPProxyRequestEvents(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprint("bypass=", bypass), func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			if bypass {
				app.cfg.Update(map[string]any{"whitelist": "127.0.0.1"})
			}
			lane := startLane(t, app, "A")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("token") != "private-value" {
					t.Error("脱敏不能改动真正转发的请求")
				}
				w.Write([]byte("ok"))
			}))
			defer srv.Close()
			for _, target := range []string{srv.URL + "/gift.mp4?token=private-value", srv.URL + "/api/room?token=private-value"} {
				if n, _, err := fetch(lane, target); err != nil || n != 2 {
					t.Fatalf("fetch = %d, %v", n, err)
				}
			}
			events := eventText(app)
			mode := "弱网"
			if bypass {
				mode = "直连"
			}
			for _, want := range []string{"[资源][代理] GET " + srv.URL + "/gift.mp4?token=REDACTED", "[HTTP][代理] GET " + srv.URL + "/api/room?token=REDACTED", " · " + mode} {
				if !strings.Contains(events, want) {
					t.Errorf("缺少事件 %q，实际 %s", want, events)
				}
			}
			if strings.Contains(events, "private-value") {
				t.Fatal("查询参数泄露到事件")
			}
		})
	}
}

// HTTPS 原样转发，不读取加密路径，也不需要在手机安装抓包证书。
func TestHTTPSProxyTunnelEvent(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := startLane(t, app, "A")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", lane.port))
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL + "/encrypted-path?secret=private-value")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, bufio.NewReader(resp.Body))
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	events := eventText(app)
	if !strings.Contains(events, "[HTTPS 隧道][代理] CONNECT ") || !strings.Contains(events, "加密路径不可见") {
		t.Fatalf("缺少 HTTPS 隧道事件：%s", events)
	}
	if strings.Contains(events, "encrypted-path") || strings.Contains(events, "private-value") {
		t.Fatal("代理不应看到 HTTPS 路径")
	}
}

func TestRequestEventFilterMetadata(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := newLane(app, "phone-A", "Same model", 0)
	id := lane.requestEvent("App 日志", "com.example.app", "GET", "https://user:password@api.example.com/a?token=secret", "")
	event := waitEventStatus(t, app, id, "unknown")
	if event.Serial != "phone-A" || event.App != "com.example.app" || event.Host != "api.example.com" {
		t.Fatalf("filter metadata: %+v", event)
	}
	if strings.Contains(event.Text, "secret") || strings.Contains(event.Text, "password") {
		t.Fatal("event leaked URL secrets")
	}
	id = lane.proxyRequestEvent("com.example.app", "CONNECT", "[2001:db8::1]:443", true)
	app.finishEvent(id, "complete", "")
	event = waitEventStatus(t, app, id, "complete")
	if event.Serial != "phone-A" || event.Host != "2001:db8::1" || event.App != "com.example.app" {
		t.Fatalf("tunnel filter metadata: %+v", event)
	}
}
