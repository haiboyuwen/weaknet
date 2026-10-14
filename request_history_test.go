package main

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRequestHistorySurvivesRealtimeEviction(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	id := l.proxyRequestEvent("com.example", "POST", "http://example.com/api?token=secret", true)
	l.requestEvent("App 日志", "com.example", "GET", "http://example.com/log", "")
	for i := 0; i < maxRealtimeEvents+1; i++ {
		a.event("tool")
	}
	n := int64(42)
	a.setEventHTTPMetadata(id, 503, "application/json", "HTTP/1.1", &n)
	a.finishEvent(id, "failed", "HTTP 503")
	a.finishEvent(id, "complete", "late")
	rows, sum, _ := a.queryRequestHistory(RequestHistoryFilter{}, 100, 0)
	if len(rows) != 1 || sum.Count != 1 || sum.Failed != 1 || sum.Down != 42 {
		t.Fatalf("history: %+v %+v", rows, sum)
	}
	e := rows[0]
	if e.ID != id || e.Status != "failed" || e.StatusCode != 503 || e.ContentType != "application/json" || e.Ended == 0 || !e.Shaped || e.Device != "Phone" || strings.Contains(e.URL, "secret") {
		t.Fatalf("invalid event: %+v", e)
	}
	if len(a.state("").EventDetails) != maxRealtimeEvents {
		t.Fatal("realtime retention changed")
	}
	rows[0].Text = "mutated"
	stored, _, _ := a.queryRequestHistory(RequestHistoryFilter{}, 100, 0)
	if stored[0].Text == "mutated" {
		t.Fatal("snapshot exposes history memory")
	}
}

func TestRequestHistoryMatchesRealtimeAndPages(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	first := l.proxyRequestEvent("com.example", "GET", "http://example.com/first", false)
	a.finishEvent(first, "complete", "")
	l.proxyRequestEvent("com.other", "CONNECT", "other.example:443", true)
	rows, sum, next := a.queryRequestHistory(RequestHistoryFilter{}, 1, 0)
	if sum.Count != 2 || len(rows) != 1 || next != rows[0].ID {
		t.Fatalf("first page: %+v %+v %d", rows, sum, next)
	}
	for _, e := range a.state("").EventDetails {
		if e.ID == first {
			filtered, _, _ := a.queryRequestHistory(RequestHistoryFilter{App: "com.example"}, 100, 0)
			if !reflect.DeepEqual(e, filtered[0]) {
				t.Fatalf("realtime/history differ: %+v %+v", e, filtered[0])
			}
		}
	}
	l.proxyRequestEvent("com.example", "GET", "http://example.com/new", false)
	older, total, _ := a.queryRequestHistory(RequestHistoryFilter{}, 1, next)
	if len(older) != 1 || older[0].ID != first || total.Count != 3 {
		t.Fatal("new request shifted older page")
	}
	for _, tc := range []struct {
		f     RequestHistoryFilter
		count int
	}{
		{RequestHistoryFilter{Status: "request_active"}, 1},
		{RequestHistoryFilter{Status: "tunnel_active"}, 1},
		{RequestHistoryFilter{Status: "complete", Shaped: "false", Host: "EXAMPLE", Query: "first", Serial: "phone", App: "com.example"}, 1},
		{RequestHistoryFilter{Type: "HTTPS 隧道"}, 1},
		{RequestHistoryFilter{Since: 1, Until: 2}, 0},
	} {
		_, s, _ := a.queryRequestHistory(tc.f, 100, 0)
		if s.Count != tc.count {
			t.Fatalf("filter %+v: %d", tc.f, s.Count)
		}
	}
}

func TestRequestHistoryKeepsEverythingUntilCleared(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	first := l.proxyRequestEvent("com.example", "GET", "http://example.com/first", false)
	for i := 0; i < maxRealtimeEvents+5; i++ {
		l.proxyRequestEvent("com.example", "GET", "http://example.com/next", false)
	}
	if _, sum, _ := a.queryRequestHistory(RequestHistoryFilter{}, 1, 0); sum.Count != maxRealtimeEvents+6 {
		t.Fatalf("history count = %d", sum.Count)
	}
	for _, e := range a.state("").EventDetails {
		if e.ID == first {
			t.Fatal("oldest event should have left the realtime list")
		}
	}
	a.clearRequestData()
	st := a.state("")
	if len(st.EventDetails) != 0 || len(st.Events) != 0 {
		t.Fatalf("realtime list not cleared: %d", len(st.EventDetails))
	}
	if _, sum, _ := a.queryRequestHistory(RequestHistoryFilter{}, 1, 0); sum.Count != 0 {
		t.Fatalf("history not cleared: %d", sum.Count)
	}
	// 进行中的旧请求之后完成不应报错，新请求 ID 继续递增。
	a.finishEvent(first, "complete", "")
	if next := l.proxyRequestEvent("com.example", "GET", "http://example.com/after", false); next <= first {
		t.Fatalf("event id reused: %d", next)
	}
}

func TestRequestHistoryRetainsCompletedInspectionSummary(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	for i := 0; i <= inspectionEntryLimit; i++ {
		id := l.requestEvent("代理", "com.example", "POST", "https://example.com/api?token=secret", " · HTTPS 解密")
		req := httptest.NewRequest("POST", "https://example.com/api", nil)
		req.Header.Set("Content-Type", "application/json")
		a.inspections.begin(id, l, "com.example", "POST", "https://example.com/api?token=REDACTED", req)
		a.inspections.read(id, false, []byte("private-request-body"), io.EOF)
		a.inspections.response(id, &http.Response{StatusCode: 201, Proto: "HTTP/1.1", Header: http.Header{"Content-Type": []string{"application/json"}}})
		a.inspections.read(id, true, []byte("private-response-body"), io.EOF)
		a.finishEvent(id, "complete", "HTTP 201")
		a.inspections.finish(id, "complete", "HTTP 201")
	}
	for i := 0; i < maxRealtimeEvents+1; i++ {
		a.event("tool")
	}
	if _, ok := a.inspections.get(1); ok {
		t.Fatal("inspection cache was not evicted")
	}
	rows, _, _ := a.queryRequestHistory(RequestHistoryFilter{Type: "decrypted"}, 0, 0)
	e := rows[len(rows)-1]
	if len(rows) != inspectionEntryLimit+1 || e.ID != 1 || e.Method != "POST" || e.StatusCode != 201 || e.RequestBytes == nil || *e.RequestBytes != 20 || e.ResponseBytes == nil || *e.ResponseBytes != 21 || e.ContentType != "application/json" || e.Ended == 0 {
		t.Fatalf("summary lost after eviction: %+v", e)
	}
	data, _ := json.Marshal(rows)
	if strings.Contains(string(data), "private-request-body") || strings.Contains(string(data), "private-response-body") || strings.Contains(string(data), "token=secret") {
		t.Fatal("history leaked headers, bodies or query secrets")
	}
}

func TestRequestHistoryRoutesCSVAndClear(t *testing.T) {
	a := newTestApp(t, "unlimited")
	l := newLane(a, "phone", "Phone", 0)
	id := l.proxyRequestEvent("com.example", "GET", "http://example.com/file?secret=hidden", false)
	n := int64(123)
	a.setEventHTTPMetadata(id, 200, "application/json", "HTTP/1.1", &n)
	a.finishEvent(id, "complete", "HTTP 200")
	l.proxyRequestEvent("com.example", "CONNECT", "example.com:443", false)
	h := a.routes()
	call := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	w := call("GET", "/api/request-history?status=complete")
	var result struct {
		Rows    []EventView `json:"rows"`
		Summary ConnSummary `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || len(result.Rows) != 1 || result.Rows[0].ID != id {
		t.Fatalf("query: %s %v", w.Body.String(), err)
	}
	w = call("GET", "/api/request-history.csv")
	if w.Code != 200 || strings.Contains(w.Body.String(), "hidden") {
		t.Fatalf("CSV: %s", w.Body.String())
	}
	cells, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil || len(cells) != 3 || cells[1][1] != "http://example.com/file?secret=REDACTED" || cells[1][6] != "200" || cells[1][12] != "123" || cells[2][6] != "" || cells[2][12] != "" {
		t.Fatalf("CSV fields: %+v %v", cells, err)
	}
	for _, path := range []string{"/api/request-history?since=oops", "/api/request-history.csv?since=20&until=1"} {
		if w := call("GET", path); w.Code != 400 {
			t.Fatalf("invalid range accepted: %s", path)
		}
	}
	if w := call("POST", "/api/request-history/clear"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	_, sum, _ := a.queryRequestHistory(RequestHistoryFilter{}, 100, 0)
	if sum.Count != 0 || len(a.state("").EventDetails) != 0 {
		t.Fatal("clear must clear both request history and the realtime list")
	}
	a.finishEvent(id, "failed", "after clear")
	_, sum, _ = a.queryRequestHistory(RequestHistoryFilter{}, 100, 0)
	if sum.Count != 0 {
		t.Fatal("finishing cleared request resurrected history")
	}
}
