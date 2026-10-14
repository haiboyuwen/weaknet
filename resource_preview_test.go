package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResourcePreviewKinds(t *testing.T) {
	for _, tc := range []struct{ url, mime, kind string }{
		{"https://files.example/a.MP4?token=REDACTED", "application/octet-stream", "video"},
		{"https://files.example/a.svga", "application/zip", "svga"},
		{"https://files.example/a.txt", "", "text"},
		{"https://files.example/a", "application/json; charset=utf-8", "text"},
		{"https://files.example/a.webp", "", "image"},
		{"https://files.example/a.mp3", "audio/mpeg", "audio"},
		{"https://files.example/a.zip", "application/zip", ""},
	} {
		if got := resourcePreviewKind(tc.url, tc.mime); got != tc.kind {
			t.Errorf("%s %s: %s", tc.url, tc.mime, got)
		}
	}
}

func TestResourcePreviewPrivateURLRangeAndNoReplayHeaders(t *testing.T) {
	requests := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || r.URL.Query().Get("signature") != "private-signature" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("preview altered URL or replayed headers: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "clip.mp4", time.Time{}, strings.NewReader("0123456789"))
	}))
	defer origin.Close()
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	id := l.requestEvent("代理", "com.example", "GET", origin.URL+"/clip.mp4?signature=private-signature", " · HTTPS 解密")
	h := a.routes()
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Cookie", "do-not-forward")
		r.Header.Set("Authorization", "Bearer do-not-forward")
		r.Header.Set("Range", "bytes=2-5")
		h.ServeHTTP(w, r)
		return w
	}
	w := call(fmt.Sprintf("/api/requests/%d/preview", id))
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-signature") || requests != 0 {
		t.Fatalf("preview metadata leaked URL or fetched origin: %s", w.Body.String())
	}
	var info map[string]string
	json.Unmarshal(w.Body.Bytes(), &info)
	if info["kind"] != "video" || info["resource_url"] != fmt.Sprintf("/api/requests/%d/resource", id) {
		t.Fatal(info)
	}
	w = call(info["resource_url"])
	if w.Code != 206 || w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" || requests != 1 {
		t.Fatalf("range: %d %s %+v", w.Code, w.Body.String(), w.Header())
	}
	if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unsafe preview headers")
	}
	data, _ := json.Marshal(a.state(""))
	rows, _, _ := a.queryRequestHistory(RequestHistoryFilter{}, 100, 0)
	history, _ := json.Marshal(rows)
	if strings.Contains(string(data), "private-signature") || strings.Contains(string(history), "private-signature") {
		t.Fatal("private URL entered state/history")
	}
	if a.state("").EventSeq != 1 {
		t.Fatal("preview polluted phone request capture")
	}
}

func TestResourcePreviewTextLimitsUnknownAndUpstreamErrors(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<script>unsafe()</script>")
		case "/big.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			io.WriteString(w, strings.Repeat("x", resourcePreviewTextLimit+1))
		case "/private.mp4":
			http.Error(w, "signature=private-origin-error", 403)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, "binary")
		}
	}))
	defer origin.Close()
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/page.html", 200}, {"/big.txt", 413}, {"/private.mp4", 502}, {"/unknown.bin", 415}} {
		id := l.requestEvent("代理", "com.example", "GET", origin.URL+tc.path, "")
		if tc.path == "/unknown.bin" {
			a.previewTargets.add(id, origin.URL+tc.path)
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/requests/%d/resource", id), nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if tc.path == "/page.html" && w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
			t.Fatal("active HTML served from local origin")
		}
		if strings.Contains(w.Body.String(), "private-origin-error") {
			t.Fatal("upstream error body leaked")
		}
	}
}

func TestResourcePreviewCacheEligibilityAndAssets(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	api := l.requestEvent("代理", "com.example", "GET", "https://api.example/account", "")
	if _, ok := a.previewTargets.get(api); ok {
		t.Fatal("ordinary API calls must not be replayed for preview")
	}
	post := l.requestEvent("代理", "com.example", "POST", "https://files.example/video.mp4", "")
	if _, ok := a.previewTargets.get(post); ok {
		t.Fatal("POST must never be replayed as GET")
	}
	log := l.requestEvent("App 日志", "com.example", "", "https://files.example/a.svga", "")
	if _, ok := a.previewTargets.get(log); !ok {
		t.Fatal("explicit resource log should be previewable")
	}
	for i := uint64(100); i < 100+resourcePreviewTargetLimit+1; i++ {
		a.previewTargets.add(i, "https://files.example/a.mp4")
	}
	if _, ok := a.previewTargets.get(100); ok {
		t.Fatal("unbounded original URL cache")
	}
	a.previewTargets.add(9000, "https://user:secret@files.example/a.txt#fragment")
	if raw, _ := a.previewTargets.get(9000); strings.Contains(raw, "secret") || strings.Contains(raw, "fragment") {
		t.Fatal("user info/fragment should be stripped")
	}
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/requests/bad/preview", 400}, {fmt.Sprintf("/api/requests/%d/preview", post), 404}, {"/assets/preview/svga-2.3.2.min.js", 200}, {"/assets/preview/jszip-3.10.1.min.js", 200}, {"/assets/preview/howler-2.2.4.min.js", 200}} {
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
	}
}
