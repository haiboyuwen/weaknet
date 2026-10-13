package main

import (
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func inspectionClient(t *testing.T, handler http.HandlerFunc) (*App, *http.Client, *httptest.Server) {
	t.Helper()
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	m.upstreamRoots = x509.NewCertPool()
	m.upstreamRoots.AddCert(srv.Certificate())
	roots := x509.NewCertPool()
	roots.AddCert(m.root)
	lane := startLane(t, app, "phone")
	client, tr := tlsProxyClient(lane, roots)
	t.Cleanup(tr.CloseIdleConnections)
	return app, client, srv
}
func inspectionAPI(t *testing.T, app *App, id uint64) RequestInspection {
	t.Helper()
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/api/requests/%d", id), nil))
	if w.Code != 200 {
		t.Fatalf("details HTTP %d: %s", w.Code, w.Body.String())
	}
	var d RequestInspection
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func TestInspectionRequestResponseAndGzip(t *testing.T) {
	payload := `{"room":123,"message":"request-body-marker"}`
	response := `{"ok":true,"message":"private-response-payload"}`
	app, client, srv := inspectionClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != payload || r.Header.Get("Authorization") != "Bearer original-secret" || r.Header.Get("Cookie") != "session=original-cookie" || r.URL.Query().Get("token") != "query-secret" {
			t.Error("inspection altered the original request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Set-Cookie", "new_session=original-response-secret")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(response))
		gz.Close()
	})
	req, _ := http.NewRequest("POST", srv.URL+"/join?token=query-secret", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer original-secret")
	req.Header.Set("Cookie", "session=original-cookie")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := io.ReadAll(gz)
	gz.Close()
	if string(original) != response || resp.Header.Get("Set-Cookie") != "new_session=original-response-secret" {
		t.Fatal("response changed")
	}
	waitEventStatus(t, app, 1, "complete")
	d := inspectionAPI(t, app, 1)
	if d.Method != "POST" || d.StatusCode != 200 || d.Request.Body != payload || d.Response.Body != response || d.Status != "complete" {
		t.Fatalf("incorrect capture %+v", d)
	}
	if !d.Request.Complete || !d.Response.Complete || d.Response.Bytes != int64(len(compressed)) {
		t.Fatalf("incorrect capture lengths %+v", d)
	}
	if strings.Contains(d.URL, "query-secret") || !strings.Contains(d.URL, "token=REDACTED") {
		t.Fatal("unredacted URL")
	}
	if d.Request.Headers.Get("Authorization") != "REDACTED" || d.Request.Headers.Get("Cookie") != "REDACTED" || d.Response.Headers.Get("Set-Cookie") != "REDACTED" {
		t.Fatal("sensitive headers not redacted")
	}
	st := app.state("")
	encoded, _ := json.Marshal(st)
	if !st.EventDetails[0].Decrypted || bytes.Contains(encoded, []byte("private-response-payload")) || bytes.Contains(encoded, []byte("request-body-marker")) {
		t.Fatal("bodies leaked into polling state")
	}
}
func TestInspectionTruncationDoesNotTruncateForwarding(t *testing.T) {
	request := strings.Repeat("r", inspectionBodyLimit+30000)
	response := strings.Repeat("s", inspectionBodyLimit+50000)
	app, client, srv := inspectionClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != request {
			t.Error("request was truncated")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(response))
	})
	resp, err := client.Post(srv.URL, "text/plain", strings.NewReader(request))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != response {
		t.Fatal("response was truncated")
	}
	waitEventStatus(t, app, 1, "complete")
	d := inspectionAPI(t, app, 1)
	for _, v := range []MessagePreview{d.Request, d.Response} {
		if !v.Truncated || len(v.Body) != inspectionBodyLimit || !v.Complete {
			t.Fatalf("unbounded or incomplete preview: len=%d %+v", len(v.Body), v)
		}
	}
	if d.Request.Bytes != int64(len(request)) || d.Response.Bytes != int64(len(response)) {
		t.Fatal("total bytes should reflect full payload")
	}
}
func TestInspectionUpdatesDuringStreaming(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	app, client, srv := inspectionClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("last\n"))
	})
	resp, err := client.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	prefix := make([]byte, 6)
	if _, err = io.ReadFull(resp.Body, prefix); err != nil {
		t.Fatal(err)
	}
	d := inspectionAPI(t, app, 1)
	if d.Status != "active" || d.Response.Body != "first\n" || d.Response.Complete {
		t.Fatalf("stream not visible %+v", d)
	}
	// Release the handler without closing the channel twice.
	release <- struct{}{}
	tail, err := io.ReadAll(resp.Body)
	if err != nil || string(tail) != "last\n" {
		t.Fatal("stream changed")
	}
	waitEventStatus(t, app, 1, "complete")
	done := inspectionAPI(t, app, 1)
	if done.Response.Body != "first\nlast\n" || done.Version <= d.Version || !done.Response.Complete {
		t.Fatalf("stream capture did not update %+v", done)
	}
}
func TestInspectionBoundedCacheAndMissingEvents(t *testing.T) {
	app := newTestApp(t, "unlimited")
	lane := newLane(app, "phone", "phone", 0)
	req := httptest.NewRequest("POST", "https://api.example/data", nil)
	for i := uint64(1); i <= inspectionEntryLimit+1; i++ {
		app.inspections.begin(i, lane, "com.example.app", "POST", "https://api.example/data", req)
		app.inspections.read(i, false, []byte("payload"), io.EOF)
		app.inspections.finish(i, "complete", "HTTP 200")
	}
	if _, ok := app.inspections.get(1); ok {
		t.Fatal("old detail not evicted")
	}
	app.inspections.read(1, true, bytes.Repeat([]byte("x"), inspectionBodyLimit), nil)
	d := inspectionAPI(t, app, 101)
	if d.Status != "complete" || d.Detail != "HTTP 200" {
		t.Fatal("finished status depends on event remaining in feed")
	}
	app.inspections.mu.Lock()
	n := len(app.inspections.entries)
	app.inspections.mu.Unlock()
	if n != inspectionEntryLimit {
		t.Fatal("unbounded cache")
	}
	for _, path := range []string{"/api/requests/1", "/api/requests/not-a-number"} {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 && w.Code != 400 {
			t.Fatalf("bad missing response %d", w.Code)
		}
	}
}
func TestInspectionBinaryCompressionAndHeaderLimits(t *testing.T) {
	binary := previewMessage(capturedMessage{headers: http.Header{"Content-Type": []string{"image/png"}}, data: []byte{1, 2, 3}, count: 3, complete: true})
	if binary.Body != "" || !strings.Contains(binary.Note, "二进制") {
		t.Fatal("binary preview")
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Write(bytes.Repeat([]byte("x"), inspectionBodyLimit*4))
	gz.Close()
	decoded := previewMessage(capturedMessage{headers: http.Header{"Content-Type": []string{"text/plain"}, "Content-Encoding": []string{"gzip"}}, data: compressed.Bytes(), count: int64(compressed.Len()), complete: true})
	if !decoded.Truncated || len(decoded.Body) != inspectionBodyLimit {
		t.Fatal("unbounded decompression")
	}
	headers, note := previewHeaders(http.Header{"X-Large": []string{strings.Repeat("x", inspectionHeaderLimit+1)}, "X-Api-Key": []string{"secret"}})
	if note == "" || headers.Get("X-Large") != "" || headers.Get("X-Api-Key") != "REDACTED" {
		t.Fatal("header retention not bounded")
	}
}
func TestHTTPSReconnectOnlyChangedTunnelTargets(t *testing.T) {
	app := newTestApp(t, "unlimited")
	var err error
	app.https, err = openHTTPSManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lane := newLane(app, "phone", "phone", 0)
	app.lanes["phone"] = lane
	records := []*ConnRecord{}
	for i, host := range []string{"api.example", "cdn.api.example", "other.example", "api.example"} {
		client, clientPeer := net.Pipe()
		upstream, upstreamPeer := net.Pipe()
		t.Cleanup(func() { client.Close(); clientPeer.Close(); upstream.Close(); upstreamPeer.Close() })
		rec := app.conns.open(lane, "com.example.app", host, "443", false)
		records = append(records, rec)
		lane.active[&connPair{client: client, upstream: upstream, record: rec, tunnel: i != 3}] = struct{}{}
	}
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/https", strings.NewReader(`{"enabled":true,"domains":["api.example"],"reconnect":true}`)))
	if w.Code != 200 {
		t.Fatalf("save failed: %s", w.Body.String())
	}
	for i, rec := range records {
		want := "active"
		if i < 2 {
			want = "failed"
		}
		if rec.snapshot().Status != want {
			t.Fatalf("wrong connection reset: %d %+v", i, rec.snapshot())
		}
	}
	lane.mu.Lock()
	n := len(lane.active)
	lane.mu.Unlock()
	if n != 2 {
		t.Fatal("unrelated connections closed")
	}
	// Re-saving unchanged settings must not reset existing decrypted connections.
	if n := app.reconnectHTTPS(app.https.status().HTTPSSettings, app.https.status().HTTPSSettings); n != 0 {
		t.Fatal("unchanged scope reset connections")
	}
}
