package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxConns 历史连接上限，超出后丢弃最早的
const maxConns = 200000

// ConnView 一条连接记录的快照（对外输出）
type ConnView struct {
	ID     int64  `json:"id"`
	Serial string `json:"serial"`
	Device string `json:"device"`
	App    string `json:"app"`
	Host   string `json:"host"`
	Port   string `json:"port"`
	Start  int64  `json:"start"` // unix 毫秒
	End    int64  `json:"end"`   // 0 表示进行中
	Up     int64  `json:"up"`    // 字节
	Down   int64  `json:"down"`  // 字节
	Shaped bool   `json:"shaped"`
	Status string `json:"status"` // active 进行中 | ok 完成 | failed 失败 | rejected 断网拒绝
	Error  string `json:"error,omitempty"`
}

// ConnRecord 一条连接的历史记录，进行中的连接字节数实时累加
type ConnRecord struct {
	ConnView
	up, down atomic.Int64
	mu       sync.Mutex
	onFinish func(string, string)
}

func (r *ConnRecord) finish(status, errMsg string) {
	r.mu.Lock()
	if r.End != 0 {
		r.mu.Unlock()
		return
	}
	r.End = time.Now().UnixMilli()
	r.Status, r.Error = status, errMsg
	callback := r.onFinish
	r.mu.Unlock()
	if callback != nil {
		callback(status, errMsg)
	}
}

func (r *ConnRecord) snapshot() ConnView {
	r.mu.Lock()
	v := r.ConnView
	r.mu.Unlock()
	v.Up, v.Down = r.up.Load(), r.down.Load()
	return v
}

type ConnStore struct {
	mu     sync.Mutex
	list   []*ConnRecord
	nextID int64
}

func (s *ConnStore) open(l *Lane, app, host, port string, shaped bool) *ConnRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	r := &ConnRecord{ConnView: ConnView{ID: s.nextID, Serial: l.serial, Device: l.name, App: app, Host: host, Port: port,
		Start: time.Now().UnixMilli(), Shaped: shaped, Status: "active"}}
	s.list = append(s.list, r)
	if len(s.list) > maxConns {
		s.list = append([]*ConnRecord(nil), s.list[len(s.list)-maxConns*9/10:]...)
	}
	return r
}

func (s *ConnStore) clear() {
	s.mu.Lock()
	s.list = nil
	s.mu.Unlock()
}

type ConnFilter struct {
	Serial, App, Host, Status string
	Shaped                    string // "true" | "false" | ""
	Since, Until              int64  // 开始时间范围，Unix 毫秒；0 表示不限
}

func (f ConnFilter) match(r *ConnRecord) bool {
	if f.Serial != "" && r.Serial != f.Serial {
		return false
	}
	if f.App == "-" { // "-" 表示未识别 App 的连接
		if r.App != "" {
			return false
		}
	} else if f.App != "" && r.App != f.App {
		return false
	}
	if f.Host != "" && !strings.Contains(strings.ToLower(r.Host), strings.ToLower(f.Host)) {
		return false
	}
	if f.Shaped != "" && r.Shaped != (f.Shaped == "true") {
		return false
	}
	if f.Since > 0 && r.Start < f.Since || f.Until > 0 && r.Start > f.Until {
		return false
	}
	return true
}

type ConnSummary struct {
	Count    int     `json:"count"`
	Active   int     `json:"active"`
	Failed   int     `json:"failed"`
	Down     int64   `json:"down"`
	Up       int64   `json:"up"`
	AvgMs    float64 `json:"avg_ms"` // 已结束连接的平均时长
	Shaped   int     `json:"shaped"`
	Bypassed int     `json:"bypassed"`
}

// query 返回符合条件的记录（最新在前，最多 limit 条）和全部匹配记录的汇总
func (s *ConnStore) query(f ConnFilter, limit int) ([]ConnView, ConnSummary) {
	rows, sum, _ := s.queryPage(f, limit, 0)
	return rows, sum
}

// 游标只限制返回的行，汇总始终覆盖全部匹配记录；新增连接不会使后续页重复或跳行。
func (s *ConnStore) queryPage(f ConnFilter, limit int, before int64) ([]ConnView, ConnSummary, int64) {
	s.mu.Lock()
	list := append([]*ConnRecord(nil), s.list...)
	s.mu.Unlock()
	var sum ConnSummary
	var doneMs, done int64
	out := []ConnView{}
	var next int64
	for i := len(list) - 1; i >= 0; i-- {
		if !f.match(list[i]) {
			continue
		}
		r := list[i].snapshot()
		if f.Status == "errors" && r.Status != "failed" && r.Status != "rejected" {
			continue
		}
		if f.Status != "" && f.Status != "errors" && r.Status != f.Status {
			continue
		}
		sum.Count++
		sum.Down += r.Down
		sum.Up += r.Up
		if r.Shaped {
			sum.Shaped++
		} else {
			sum.Bypassed++
		}
		switch r.Status {
		case "active":
			sum.Active++
		case "failed", "rejected":
			sum.Failed++
		}
		if r.End != 0 {
			doneMs += r.End - r.Start
			done++
		}
		if before > 0 && r.ID >= before {
			continue
		}
		if limit <= 0 || len(out) < limit {
			out = append(out, r)
		} else if next == 0 {
			next = out[len(out)-1].ID
		}
	}
	if done > 0 {
		sum.AvgMs = float64(doneMs) / float64(done)
	}
	return out, sum, next
}

// AppSummary 每台手机上每个 App 的累计数据，用于筛选下拉框
type AppSummary struct {
	Serial   string `json:"serial"`
	Device   string `json:"device"`
	App      string `json:"app"`
	Conns    int    `json:"conns"`
	Down     int64  `json:"down"`
	LastSeen int64  `json:"last_seen"`
}

func (s *ConnStore) apps() []AppSummary {
	s.mu.Lock()
	list := append([]*ConnRecord(nil), s.list...)
	s.mu.Unlock()
	m := map[string]*AppSummary{}
	for _, r := range list {
		k := r.Serial + "\t" + r.App
		a := m[k]
		if a == nil {
			a = &AppSummary{Serial: r.Serial, Device: r.Device, App: r.App}
			m[k] = a
		}
		a.Conns++
		a.Down += r.down.Load()
		if r.Start > a.LastSeen {
			a.LastSeen = r.Start
		}
	}
	out := make([]AppSummary, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Down > out[j].Down })
	return out
}

func (s *ConnStore) writeCSV(w io.Writer, f ConnFilter) error {
	rows, _ := s.query(f, 0)
	cw := csv.NewWriter(w)
	cw.Write([]string{"开始时间", "结束时间", "耗时(ms)", "手机", "序列号", "App", "域名", "端口", "下行(B)", "上行(B)", "走弱网", "状态", "错误"})
	for i := len(rows) - 1; i >= 0; i-- { // 按时间正序导出
		r := rows[i]
		end, dur := "", ""
		if r.End != 0 {
			end = time.UnixMilli(r.End).Format("2006-01-02 15:04:05.000")
			dur = fmt.Sprint(r.End - r.Start)
		}
		cw.Write([]string{time.UnixMilli(r.Start).Format("2006-01-02 15:04:05.000"), end, dur, r.Device, r.Serial,
			r.App, r.Host, r.Port, fmt.Sprint(r.Down), fmt.Sprint(r.Up), fmt.Sprint(r.Shaped), r.Status, r.Error})
	}
	cw.Flush()
	return cw.Error()
}
