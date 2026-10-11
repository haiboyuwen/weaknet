package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
