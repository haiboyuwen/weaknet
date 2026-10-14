package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Producers only signal a bounded channel; a slow console never blocks forwarding.
type stateUpdates struct {
	mu        sync.Mutex
	listeners map[chan struct{}]struct{}
	revision  atomic.Uint64
}

func (s *stateUpdates) notify() {
	s.revision.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.listeners {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *stateUpdates) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.listeners == nil {
		s.listeners = make(map[chan struct{}]struct{})
	}
	s.listeners[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() { s.mu.Lock(); delete(s.listeners, ch); s.mu.Unlock() }
}

func (a *App) stateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "流式响应不可用", http.StatusInternalServerError)
		return
	}
	updates, unsubscribe := a.updates.subscribe()
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	app := r.URL.Query().Get("app")
	var cost time.Duration // 上一次含请求列表的推送耗时，用于拉长合并间隔
	write := func(kind string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	// 完整的 1 万条事件约 8 MB，浏览器 EventSource 处理数 MB 的单条消息会长时间卡住甚至收不到，
	// 所以只推送本连接尚未发过或内容有变化的事件，并按 eventChunk 条分多条消息发送。
	var sent map[uint64]uint64
	send := func(kind string, withRequests bool) bool {
		if !withRequests {
			return write(kind, a.stateSnapshot(app, false))
		}
		begin := time.Now()
		defer func() { cost = time.Since(begin) }()
		st := a.stateSnapshot(app, true)
		upserts, seen := eventDelta(st.EventDetails, sent)
		st.EventDelta, st.EventReset = true, sent == nil
		st.EventKeepFrom = st.EventSeq + 1
		if n := len(st.EventDetails); n > 0 {
			st.EventKeepFrom = st.EventDetails[n-1].ID
		}
		st.Events, st.EventDetails = nil, nil
		sent = seen
		first := min(len(upserts), eventChunk)
		st.EventUpserts, st.EventPartial = upserts[:first], first < len(upserts)
		if !write(kind, st) {
			return false
		}
		for rest := upserts[first:]; len(rest) > 0; {
			n := min(len(rest), eventChunk)
			chunk := State{Revision: st.Revision, EventSeq: st.EventSeq, EventDelta: true, EventKeepFrom: st.EventKeepFrom, EventUpserts: rest[:n], EventPartial: n < len(rest)}
			if !write("events", chunk) {
				return false
			}
			rest = rest[n:]
		}
		return true
	}
	if !send("state", true) {
		return
	}
	// Rate and queue gauges are sampled separately, without repeating request data.
	metrics := time.NewTicker(time.Second)
	defer metrics.Stop()
	var pending *time.Timer
	var ready <-chan time.Time
	defer func() {
		if pending != nil {
			pending.Stop()
		}
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.quit:
			return
		case <-updates:
			if ready == nil {
				pending = time.NewTimer(max(50*time.Millisecond, 3*cost))
				ready = pending.C
			}
		case <-ready:
			ready = nil
			if !send("state", true) {
				return
			}
		case <-metrics.C:
			if !send("metrics", false) {
				return
			}
		}
	}
}

// 每条推送消息最多携带的事件数，单条消息约 0.5 MB。
const eventChunk = 500

// eventDelta 返回相对上次发送（sent，为 nil 表示首次）新增或内容变化的事件，以及本次各事件的内容哈希。
// 进行中请求的耗时每次都在变，由页面按开始时间自行计算，不计入比较也不下发。
func eventDelta(events []EventView, sent map[uint64]uint64) ([]EventView, map[uint64]uint64) {
	seen := make(map[uint64]uint64, len(events))
	upserts := []EventView{}
	for _, e := range events {
		if e.Status == "active" {
			e.DurationMS = 0
		}
		data, _ := json.Marshal(e)
		h := fnv.New64a()
		h.Write(data)
		sum := h.Sum64()
		seen[e.ID] = sum
		if previous, ok := sent[e.ID]; !ok || previous != sum {
			upserts = append(upserts, e)
		}
	}
	return upserts, seen
}
