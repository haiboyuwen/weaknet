package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type streamFrame struct {
	kind  string
	state State
	err   error
}

func readStreamFrame(r *bufio.Reader) streamFrame {
	frame := streamFrame{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			frame.err = err
			return frame
		}
		if strings.HasPrefix(line, "event: ") {
			frame.kind = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			frame.err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame.state)
		}
		if line == "\n" && frame.kind != "" {
			return frame
		}
	}
}

func TestStateStreamHooksAndDisconnect(t *testing.T) {
	a := newTestApp(t, "4g")
	server := httptest.NewServer(a.routes())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/state/stream?app=com.example", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal(resp.Header)
	}
	reader := bufio.NewReader(resp.Body)
	initial := readStreamFrame(reader)
	if initial.err != nil || initial.kind != "state" {
		t.Fatalf("initial: %+v", initial)
	}
	id := a.recordEvent(EventView{Scope: "http", Method: "GET", URL: "http://example.test/file.txt", Status: "active"})
	next := func() State {
		t.Helper()
		for {
			frame := readStreamFrame(reader)
			if frame.err != nil {
				t.Fatal(frame.err)
			}
			if frame.kind == "state" {
				return frame.state
			}
		}
	}
	st := next()
	if len(st.EventDetails) != 1 || st.EventDetails[0].ID != id || st.EventDetails[0].Status != "active" {
		t.Fatalf("request missing: %+v", st.EventDetails)
	}
	// Response headers and streaming bytes trigger updates before completion.
	lane := &Lane{serial: "phone", name: "Phone"}
	r, _ := http.NewRequest("GET", "http://example.test/file.txt", nil)
	a.inspections.begin(id, lane, "com.example", "GET", r.URL.String(), r)
	a.inspections.response(id, &http.Response{StatusCode: 200, Proto: "HTTP/1.1", Header: http.Header{"Content-Type": {"text/plain"}}})
	a.inspections.read(id, true, []byte("hello"), io.EOF)
	st = next()
	if st.EventDetails[0].StatusCode != 200 || st.EventDetails[0].ResponseBytes == nil || *st.EventDetails[0].ResponseBytes != 5 {
		t.Fatalf("streaming summary missing: %+v", st.EventDetails[0])
	}
	a.finishEvent(id, "complete", "")
	st = next()
	if st.EventDetails[0].Status != "complete" {
		t.Fatalf("finish missing: %+v", st.EventDetails[0])
	}
	for {
		frame := readStreamFrame(reader)
		if frame.err != nil {
			t.Fatal(frame.err)
		}
		if frame.kind == "metrics" {
			if frame.state.EventDetails != nil || frame.state.Events != nil {
				t.Fatal("metrics repeated request data")
			}
			break
		}
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.updates.mu.Lock()
		n := len(a.updates.listeners)
		a.updates.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("disconnected listener leaked")
}

func TestStateUpdatesCoalesceWithoutBlockingProducer(t *testing.T) {
	var updates stateUpdates
	first, unsubscribeFirst := updates.subscribe()
	second, unsubscribeSecond := updates.subscribe()
	defer unsubscribeSecond()
	for i := 0; i < 10000; i++ {
		updates.notify()
	}
	for _, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		default:
			t.Fatal("listener not notified")
		}
		select {
		case <-ch:
			t.Fatal("unbounded notifications")
		default:
		}
	}
	unsubscribeFirst()
	updates.notify()
	select {
	case <-first:
		t.Fatal("notified disconnected listener")
	default:
	}
	if updates.revision.Load() != 10001 {
		t.Fatal("revision missing")
	}
}
