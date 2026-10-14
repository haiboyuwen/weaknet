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
	if initial.err != nil || initial.kind != "state" || !initial.state.EventDelta || !initial.state.EventReset || initial.state.EventDetails != nil {
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
	if len(st.EventUpserts) != 1 || st.EventUpserts[0].ID != id || st.EventUpserts[0].Status != "active" || st.EventReset || st.EventDetails != nil {
		t.Fatalf("request missing: %+v", st.EventUpserts)
	}
	// Response headers and streaming bytes trigger updates before completion.
	lane := &Lane{serial: "phone", name: "Phone"}
	r, _ := http.NewRequest("GET", "http://example.test/file.txt", nil)
	a.inspections.begin(id, lane, "com.example", "GET", r.URL.String(), r)
	a.inspections.response(id, &http.Response{StatusCode: 200, Proto: "HTTP/1.1", Header: http.Header{"Content-Type": {"text/plain"}}})
	a.inspections.read(id, true, []byte("hello"), io.EOF)
	st = next()
	for len(st.EventUpserts) == 0 || st.EventUpserts[0].ResponseBytes == nil {
		st = next()
	}
	if st.EventUpserts[0].StatusCode != 200 || *st.EventUpserts[0].ResponseBytes != 5 {
		t.Fatalf("streaming summary missing: %+v", st.EventUpserts[0])
	}
	a.finishEvent(id, "complete", "")
	st = next()
	for len(st.EventUpserts) == 0 {
		st = next()
	}
	if st.EventUpserts[0].Status != "complete" {
		t.Fatalf("finish missing: %+v", st.EventUpserts[0])
	}
	for {
		frame := readStreamFrame(reader)
		if frame.err != nil {
			t.Fatal(frame.err)
		}
		if frame.kind == "metrics" {
			if frame.state.EventDetails != nil || frame.state.Events != nil || frame.state.EventUpserts != nil {
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

func TestEventDeltaSkipsUnchangedAndActiveDuration(t *testing.T) {
	events := []EventView{{ID: 3, Status: "active", Started: 1, DurationMS: 50}, {ID: 2, Status: "complete"}, {ID: 1, Text: "tool"}}
	upserts, sent := eventDelta(events, nil)
	if len(upserts) != 3 || upserts[0].DurationMS != 0 {
		t.Fatalf("first send should include all events without live duration: %+v", upserts)
	}
	events[0].DurationMS = 900
	if upserts, sent = eventDelta(events, sent); len(upserts) != 0 {
		t.Fatalf("growing duration alone must not resend: %+v", upserts)
	}
	events[1].Status = "failed"
	events = append([]EventView{{ID: 4, Status: "complete"}}, events...)
	if upserts, _ = eventDelta(events, sent); len(upserts) != 2 || upserts[0].ID != 4 || upserts[1].ID != 2 {
		t.Fatalf("only new and changed events expected: %+v", upserts)
	}
}

func TestStateStreamChunksLargeLists(t *testing.T) {
	a := newTestApp(t, "4g")
	for i := 0; i < eventChunk*2+10; i++ {
		a.event("tool event")
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/state/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	total, frames := 0, 0
	for {
		frame := readStreamFrame(reader)
		if frame.err != nil {
			t.Fatal(frame.err)
		}
		if frame.kind == "metrics" {
			continue
		}
		frames++
		if want := map[bool]string{true: "state", false: "events"}[frames == 1]; frame.kind != want || len(frame.state.EventUpserts) > eventChunk {
			t.Fatalf("frame %d: kind %s with %d events", frames, frame.kind, len(frame.state.EventUpserts))
		}
		total += len(frame.state.EventUpserts)
		if !frame.state.EventPartial {
			break
		}
	}
	if frames != 3 || total != eventChunk*2+10 {
		t.Fatalf("got %d frames with %d events", frames, total)
	}
	a.clearRequestData()
	for {
		frame := readStreamFrame(reader)
		if frame.err != nil {
			t.Fatal(frame.err)
		}
		if frame.kind == "state" {
			if len(frame.state.EventUpserts) != 0 || frame.state.EventKeepFrom != frame.state.EventSeq+1 {
				t.Fatalf("clear should tell the page to drop every event: %+v", frame.state)
			}
			return
		}
	}
}
