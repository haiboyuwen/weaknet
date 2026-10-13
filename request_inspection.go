package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const inspectionBodyLimit = 64 << 10
const inspectionHeaderLimit = 16 << 10
const inspectionEntryLimit = 100

type MessagePreview struct {
	Headers   http.Header `json:"headers"`
	Body      string      `json:"body"`
	Bytes     int64       `json:"bytes"`
	Truncated bool        `json:"truncated"`
	Complete  bool        `json:"complete"`
	Note      string      `json:"note,omitempty"`
}
type RequestInspection struct {
	ID         uint64         `json:"id"`
	Method     string         `json:"method"`
	URL        string         `json:"url"`
	Serial     string         `json:"serial"`
	Device     string         `json:"device"`
	App        string         `json:"app"`
	Started    int64          `json:"started"`
	Ended      int64          `json:"ended,omitempty"`
	DurationMS int64          `json:"duration_ms"`
	Status     string         `json:"status"`
	Detail     string         `json:"detail,omitempty"`
	StatusCode int            `json:"status_code"`
	Protocol   string         `json:"protocol,omitempty"`
	Version    uint64         `json:"version"`
	Request    MessagePreview `json:"request"`
	Response   MessagePreview `json:"response"`
}
type capturedMessage struct {
	headers  http.Header
	data     []byte
	count    int64
	complete bool
	note     string
}
type inspectionEntry struct {
	view              RequestInspection
	request, response capturedMessage
}
type inspectionStore struct {
	mu      sync.Mutex
	entries map[uint64]*inspectionEntry
	order   []uint64
}

func previewHeaders(h http.Header) (http.Header, string) {
	result := http.Header{}
	size := 0
	note := ""
	for name, values := range h {
		key := strings.ToLower(name)
		if key == "authorization" || key == "proxy-authorization" || key == "cookie" || key == "set-cookie" || strings.Contains(key, "token") || strings.Contains(key, "api-key") || strings.Contains(key, "secret") {
			values = []string{"REDACTED"}
		}
		for _, value := range values {
			n := len(name) + len(value)
			if size+n > inspectionHeaderLimit {
				note = "请求头 / 响应头超过 16 KB，部分未保留"
				continue
			}
			result.Add(name, value)
			size += n
		}
	}
	return result, note
}
func (s *inspectionStore) begin(id uint64, l *Lane, app, method, address string, r *http.Request) {
	headers := r.Header.Clone()
	headers.Set("Host", r.Host)
	headers, note := previewHeaders(headers)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[uint64]*inspectionEntry{}
	}
	e := &inspectionEntry{view: RequestInspection{ID: id, Method: method, URL: address, Serial: l.serial, Device: l.name, App: app, Started: time.Now().UnixMilli(), Status: "active", Version: 1}, request: capturedMessage{headers: headers, note: note, complete: r.Body == nil || r.Body == http.NoBody}}
	s.entries[id] = e
	s.order = append(s.order, id)
	if len(s.order) > inspectionEntryLimit {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
}
func (s *inspectionStore) response(id uint64, r *http.Response) {
	headers, note := previewHeaders(r.Header)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		e.view.StatusCode = r.StatusCode
		e.view.Protocol = r.Proto
		e.view.Version++
		e.response = capturedMessage{headers: headers, note: note, complete: r.Body == nil || r.Body == http.NoBody}
		if r.StatusCode == 101 {
			e.response.note = "协议已升级，详情仅保留握手头，不采集 WebSocket 消息"
			e.response.complete = true
		}
	}
}
func (s *inspectionStore) read(id uint64, response bool, p []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return
	}
	m := &e.request
	if response {
		m = &e.response
	}
	m.count += int64(len(p))
	remain := inspectionBodyLimit - len(m.data)
	if len(p) > remain {
		p = p[:remain]
	}
	m.data = append(m.data, p...)
	if err == io.EOF {
		m.complete = true
	}
	e.view.Version++
}
func (s *inspectionStore) finish(id uint64, status, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		e.view.Status, e.view.Detail = status, detail
		e.view.Ended = max(e.view.Started, time.Now().UnixMilli())
		e.view.DurationMS = e.view.Ended - e.view.Started
		e.view.Version++
	}
}

type inspectionBody struct {
	io.ReadCloser
	store    *inspectionStore
	id       uint64
	response bool
}

func (b *inspectionBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.store.read(b.id, b.response, p[:n], err)
	return n, err
}
func previewMessage(m capturedMessage) MessagePreview {
	view := MessagePreview{Headers: m.headers, Bytes: m.count, Truncated: m.count > int64(len(m.data)), Complete: m.complete, Note: m.note}
	if len(m.data) == 0 {
		return view
	}
	data := m.data
	encoding := strings.ToLower(strings.TrimSpace(m.headers.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		var reader io.ReadCloser
		var err error
		switch encoding {
		case "gzip":
			reader, err = gzip.NewReader(bytes.NewReader(data))
		case "deflate":
			reader, err = zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				reader = flate.NewReader(bytes.NewReader(data))
				err = nil
			}
		default:
			view.Note += fmt.Sprintf(" %s 压缩正文暂不预览", encoding)
			return view
		}
		if err != nil {
			view.Note += " 压缩正文尚不完整，暂不预览"
			return view
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, inspectionBodyLimit+1))
		reader.Close()
		if len(decoded) > inspectionBodyLimit {
			decoded = decoded[:inspectionBodyLimit]
			view.Truncated = true
		}
		if err != nil {
			view.Note += " 压缩正文尚不完整，显示已解码部分"
		}
		data = decoded
	}
	kind, _, _ := mime.ParseMediaType(m.headers.Get("Content-Type"))
	textual := strings.HasPrefix(kind, "text/") || strings.Contains(kind, "json") || strings.Contains(kind, "xml") || strings.Contains(kind, "javascript") || kind == "application/x-www-form-urlencoded" || kind == "application/graphql"
	if kind == "" {
		textual = !bytes.Contains(data, []byte{0}) && utf8.Valid(data)
	}
	if !textual {
		view.Note += " 二进制正文不显示，转发数据保持完整"
		return view
	}
	// A byte limit may cut through the last UTF-8 character.
	view.Body = strings.ToValidUTF8(string(data), "�")
	return view
}
func (s *inspectionStore) get(id uint64) (RequestInspection, bool) {
	s.mu.Lock()
	e := s.entries[id]
	if e == nil {
		s.mu.Unlock()
		return RequestInspection{}, false
	}
	view := e.view
	req, resp := e.request, e.response
	req.headers = req.headers.Clone()
	resp.headers = resp.headers.Clone()
	req.data = append([]byte(nil), req.data...)
	resp.data = append([]byte(nil), resp.data...)
	s.mu.Unlock()
	view.Request = previewMessage(req)
	view.Response = previewMessage(resp)
	return view, true
}
func (a *App) inspectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			http.Error(w, "无效请求 ID", 400)
			return
		}
		view, ok := a.inspections.get(id)
		if !ok {
			http.Error(w, "请求详情已过期或未采集；仅保留最近 100 个已解密请求", 404)
			return
		}
		a.evMu.Lock()
		for _, e := range a.eventDetails {
			if e.ID == id {
				view.Status = e.Status
				view.Detail = e.Detail
				if e.Started > 0 {
					view.Started, view.Ended, view.DurationMS = e.Started, e.Ended, e.DurationMS
				}
				break
			}
		}
		a.evMu.Unlock()
		if view.Status == "active" && view.Started > 0 {
			view.DurationMS = max(int64(0), time.Now().UnixMilli()-view.Started)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(view)
	})
}
