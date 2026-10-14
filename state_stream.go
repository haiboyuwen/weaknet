package main

import (
	"encoding/json"
	"fmt"
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
	send := func(kind string, withRequests bool) bool {
		begin := time.Now()
		if withRequests {
			defer func() { cost = time.Since(begin) }()
		}
		data, err := json.Marshal(a.stateSnapshot(app, withRequests))
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
