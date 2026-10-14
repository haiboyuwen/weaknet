package main

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// asyncLog 让日志由后台 goroutine 写到终端：Windows 控制台写入慢，用户在窗口里选中文字时还会暂停输出，
// 同步写会拖住请求转发和控制台接口。缓冲满时丢弃新行，之后补一行提示省略了多少行。
type asyncLog struct {
	out     io.Writer
	lines   chan []byte
	dropped atomic.Int64
	idle    sync.WaitGroup
}

func newAsyncLog(out io.Writer, buffer int) *asyncLog {
	l := &asyncLog{out: out, lines: make(chan []byte, buffer)}
	go l.run()
	return l
}

func (l *asyncLog) Write(p []byte) (int, error) {
	l.idle.Add(1)
	select {
	case l.lines <- append([]byte(nil), p...):
	default:
		l.idle.Done()
		l.dropped.Add(1)
	}
	return len(p), nil
}

func (l *asyncLog) run() {
	for line := range l.lines {
		if n := l.dropped.Swap(0); n > 0 {
			fmt.Fprintf(l.out, "（终端输出过慢，省略了 %d 行日志）\n", n)
		}
		l.out.Write(line)
		l.idle.Done()
	}
}

// flush 等待已缓冲的日志写完，最多等 timeout；退出前调用，避免最后几行丢失。
func (l *asyncLog) flush(timeout time.Duration) {
	done := make(chan struct{})
	go func() { l.idle.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}
