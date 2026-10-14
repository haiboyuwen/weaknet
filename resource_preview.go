package main

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web_assets/*
var resourcePreviewAssets embed.FS

const resourcePreviewTargetLimit = 1000
const resourcePreviewTextLimit = 2 << 20
const resourcePreviewBinaryLimit = 32 << 20
const resourcePreviewMediaLimit = 512 << 20

// Original GET URLs may contain signatures. Keep this bounded cache private;
// polling, history, CSV, logs and preview metadata only expose redacted URLs.
type resourcePreviewStore struct {
	mu    sync.Mutex
	urls  map[uint64]string
	order []uint64
}

func (s *resourcePreviewStore) add(id uint64, raw string) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.urls == nil {
		s.urls = map[uint64]string{}
	}
	if _, ok := s.urls[id]; !ok {
		s.order = append(s.order, id)
	}
	s.urls[id] = u.String()
	if len(s.order) > resourcePreviewTargetLimit {
		delete(s.urls, s.order[0])
		s.order = s.order[1:]
	}
}
func (s *resourcePreviewStore) get(id uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.urls[id]
	return raw, ok
}

func resourceFileHost(host string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(host), func(r rune) bool { return r == '.' || r == '-' }) {
		if strings.HasPrefix(part, "files") {
			return true
		}
	}
	return false
}

func resourcePreviewKind(raw, contentType string) string {
	t, _, _ := mime.ParseMediaType(contentType)
	t = strings.ToLower(t)
	switch {
	case strings.HasPrefix(t, "image/"):
		return "image"
	case strings.HasPrefix(t, "video/"):
		return "video"
	case strings.HasPrefix(t, "audio/"):
		return "audio"
	case strings.HasPrefix(t, "text/") || strings.Contains(t, "json") || strings.Contains(t, "xml") || strings.Contains(t, "javascript") || t == "application/graphql":
		return "text"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".svga":
		return "svga"
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".bmp", ".ico":
		return "image"
	case ".mp4", ".m4v", ".mov", ".webm", ".ogv":
		return "video"
	case ".mp3", ".aac", ".m4a", ".wav", ".ogg", ".flac":
		return "audio"
	case ".txt", ".log", ".json", ".xml", ".csv", ".tsv", ".md", ".js", ".css", ".html", ".htm", ".yaml", ".yml", ".srt", ".vtt", ".graphql":
		return "text"
	}
	return ""
}

func (a *App) previewContentType(id uint64) string {
	summary, ok := a.inspections.summary(id)
	if ok && summary.contentType != "" {
		return summary.contentType
	}
	a.evMu.Lock()
	defer a.evMu.Unlock()
	if e := a.requestHistory.records[id]; e != nil {
		return e.ContentType
	}
	return ""
}

func (a *App) resourcePreviewRoutes(mux *http.ServeMux) {
	assets, _ := fs.Sub(resourcePreviewAssets, "web_assets")
	mux.Handle("GET /assets/preview/", http.StripPrefix("/assets/preview/", http.FileServer(http.FS(assets))))
	target := func(w http.ResponseWriter, r *http.Request) (uint64, string, bool) {
		id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "无效请求 ID", 400)
			return 0, "", false
		}
		raw, ok := a.previewTargets.get(id)
		if !ok {
			http.Error(w, "原资源地址未保留或已过期，请重新发起该资源请求后预览", 404)
			return 0, "", false
		}
		return id, raw, true
	}
	mux.HandleFunc("GET /api/requests/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		id, raw, ok := target(w, r)
		if !ok {
			return
		}
		kind := resourcePreviewKind(raw, a.previewContentType(id))
		if kind == "" {
			http.Error(w, "此资源类型暂不支持预览", 415)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]string{"kind": kind, "resource_url": "/api/requests/" + strconv.FormatUint(id, 10) + "/resource"})
	})
	mux.HandleFunc("GET /api/requests/{id}/resource", func(w http.ResponseWriter, r *http.Request) {
		_, raw, ok := target(w, r)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, r.Method, raw, nil)
		if err != nil {
			http.Error(w, "资源地址无效", 400)
			return
		}
		if value := r.Header.Get("Range"); value != "" {
			req.Header.Set("Range", value)
		}
		if value := r.Header.Get("If-Range"); value != "" {
			req.Header.Set("If-Range", value)
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(a.resolve(host), port))
		}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, ForceAttemptHTTP2: true}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return http.ErrUseLastResponse
			}
			return nil
		}}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "资源加载失败，请检查资源服务器与网络连接", 502)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			w.Header().Set("Content-Range", resp.Header.Get("Content-Range"))
			w.WriteHeader(resp.StatusCode)
			return
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			http.Error(w, "资源服务器返回 HTTP "+strconv.Itoa(resp.StatusCode)+"，资源可能需要登录或签名已过期", 502)
			return
		}
		contentType := resp.Header.Get("Content-Type")
		kind := resourcePreviewKind(raw, contentType)
		if kind == "" {
			http.Error(w, "此资源类型暂不支持预览", 415)
			return
		}
		limit := int64(resourcePreviewBinaryLimit)
		if kind == "text" {
			limit = resourcePreviewTextLimit
		}
		if kind == "video" || kind == "audio" {
			limit = resourcePreviewMediaLimit
		}
		if resp.ContentLength > limit {
			http.Error(w, "资源超过预览大小限制", 413)
			return
		}
		// No remote cookies, redirect URLs or active HTML/JS enter the local UI.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
		w.Header().Set("Content-Disposition", "inline")
		if kind == "text" {
			contentType = "text/plain; charset=utf-8"
		}
		if contentType == "" || strings.HasPrefix(contentType, "application/octet-stream") {
			u, _ := url.Parse(raw)
			if inferred := mime.TypeByExtension(path.Ext(u.Path)); inferred != "" && kind != "svga" {
				contentType = inferred
			}
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		if kind == "video" || kind == "audio" {
			for _, key := range []string{"Accept-Ranges", "Content-Range", "ETag", "Last-Modified"} {
				if value := resp.Header.Get(key); value != "" {
					w.Header().Set(key, value)
				}
			}
			if resp.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			}
			w.WriteHeader(resp.StatusCode)
			if r.Method != "HEAD" {
				io.Copy(w, io.LimitReader(resp.Body, limit))
			}
			return
		}
		if r.Method == "HEAD" {
			w.WriteHeader(resp.StatusCode)
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			http.Error(w, "资源加载中断，请重试", 502)
			return
		}
		if int64(len(data)) > limit {
			http.Error(w, "资源超过预览大小限制", 413)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(resp.StatusCode)
		w.Write(data)
	})
}
