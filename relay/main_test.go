package main

import (
	"strings"
	"testing"
)

// 摘自真机 /proc/net/tcp6：App(uid 10685) 的 socket 本地端口 0xA1B2 连向 127.0.0.1:8888(0x22B8)，
// 以及中转程序一侧 accept 得到的 socket（本地 8888，uid 2000）
const sample = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100007F:22B8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2000        0 1 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:A1B2 0000000000000000FFFF00000100007F:22B8 01 00000000:00000000 00:00000000 00000000 10685        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0000000000000000FFFF00000100007F:22B8 0000000000000000FFFF00000100007F:A1B2 01 00000000:00000000 00:00000000 00000000  2000        0 3 1 0000000000000000 20 4 30 10 -1
`

func TestParseUID(t *testing.T) {
	if uid := parseUID(strings.NewReader(sample), 0xA1B2, 8888); uid != 10685 {
		t.Fatalf("uid = %d, want 10685", uid)
	}
	if uid := parseUID(strings.NewReader(sample), 0x1234, 8888); uid != -1 {
		t.Fatalf("unknown port should be -1, got %d", uid)
	}
}
