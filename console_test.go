package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

type slowWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	release chan struct{}
}

func (w *slowWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestAsyncLogNeverBlocksCallers(t *testing.T) {
	w := &slowWriter{release: make(chan struct{})}
	l := newAsyncLog(w, 2)
	start := time.Now()
	for i := 0; i < 50; i++ {
		l.Write([]byte("line\n"))
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("a stalled console must not block log callers")
	}
	close(w.release)
	l.flush(time.Second)
	l.Write([]byte("after\n"))
	l.flush(time.Second)
	w.mu.Lock()
	out := w.buf.String()
	w.mu.Unlock()
	if !strings.Contains(out, "省略了") || !strings.HasSuffix(out, "after\n") {
		t.Fatalf("dropped lines should be reported and later lines written: %q", out)
	}
}
