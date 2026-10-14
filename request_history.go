package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Protected by App.evMu. History is unbounded and only dropped when the user
// clears it; only lightweight proxy request metadata is retained, never headers
// or bodies.
type requestHistory struct {
	records map[uint64]*EventView
	order   []uint64
}

func (h *requestHistory) add(e EventView) {
	if e.Scope != "http" && e.Scope != "tunnel" {
		return
	}
	if h.records == nil {
		h.records = make(map[uint64]*EventView)
	}
	h.order = append(h.order, e.ID)
	h.records[e.ID] = &e
}

type RequestHistoryFilter struct {
	Serial, App, Host, Status, Type, Query, Shaped string
	Since, Until                                   int64
}

func (f RequestHistoryFilter) match(e EventView) bool {
	if f.Serial != "" && e.Serial != f.Serial {
		return false
	}
	if f.App == "-" {
		if e.App != "" && e.App != "未识别 App" {
			return false
		}
	} else if f.App != "" && e.App != f.App {
		return false
	}
	if f.Host != "" && !strings.Contains(strings.ToLower(e.Host), strings.ToLower(f.Host)) {
		return false
	}
	if f.Shaped != "" && e.Shaped != (f.Shaped == "true") {
		return false
	}
	started := e.Started
	if started == 0 {
		started = e.RecordedAt
	}
	if f.Since > 0 && started < f.Since || f.Until > 0 && started > f.Until {
		return false
	}
	if f.Type == "decrypted" {
		if !e.Decrypted {
			return false
		}
	} else if f.Type != "" && !strings.Contains(e.Text, "["+f.Type+"]") {
		return false
	}
	switch f.Status {
	case "":
	case "request_active":
		if e.Status != "active" || e.Scope != "http" {
			return false
		}
	case "tunnel_active":
		if e.Status != "active" || e.Scope != "tunnel" {
			return false
		}
	case "errors":
		if e.Status != "failed" && e.Status != "timeout" {
			return false
		}
	default:
		if e.Status != f.Status {
			return false
		}
	}
	return f.Query == "" || strings.Contains(strings.ToLower(e.Text+" "+e.URL+" "+e.Detail), strings.ToLower(f.Query))
}

func (a *App) queryRequestHistory(f RequestHistoryFilter, limit int, before uint64) ([]EventView, ConnSummary, uint64) {
	summaries := a.inspections.summaries()
	a.evMu.Lock()
	list := make([]EventView, 0, len(a.requestHistory.order))
	h := &a.requestHistory
	for i := len(h.order) - 1; i >= 0; i-- {
		id := h.order[i]
		list = append(list, *h.records[id])
	}
	a.evMu.Unlock()
	rows := []EventView{}
	var sum ConnSummary
	var next uint64
	var done, duration int64
	now := time.Now().UnixMilli()
	for _, e := range list {
		if summary, ok := summaries[e.ID]; ok {
			applyRequestSummary(&e, summary)
		}
		if e.Status == "active" && e.Started > 0 {
			e.DurationMS = max(int64(0), now-e.Started)
		}
		if !f.match(e) {
			continue
		}
		sum.Count++
		if e.Status == "active" {
			sum.Active++
		}
		if e.Status == "failed" || e.Status == "timeout" {
			sum.Failed++
		}
		if e.RequestBytes != nil {
			sum.Up += *e.RequestBytes
		}
		if e.ResponseBytes != nil {
			sum.Down += *e.ResponseBytes
		}
		if e.Shaped {
			sum.Shaped++
		} else {
			sum.Bypassed++
		}
		if e.Ended > 0 {
			done++
			duration += e.DurationMS
		}
		if before > 0 && e.ID >= before {
			continue
		}
		if limit <= 0 || len(rows) < limit {
			rows = append(rows, e)
		} else if next == 0 {
			next = rows[len(rows)-1].ID
		}
	}
	if done > 0 {
		sum.AvgMs = float64(duration) / float64(done)
	}
	return rows, sum, next
}

func (a *App) writeRequestHistoryCSV(w io.Writer, f RequestHistoryFilter) error {
	rows, _, _ := a.queryRequestHistory(f, 0, 0)
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"请求 ID", "网址", "手机", "序列号", "App", "方法", "HTTP 状态", "状态", "开始时间", "结束时间", "耗时(ms)", "请求正文(B)", "响应正文(B)", "SSL", "内容类型", "走弱网", "说明"}); err != nil {
		return err
	}
	clock := func(t int64) string {
		if t == 0 {
			return ""
		}
		return time.UnixMilli(t).Format("2006-01-02 15:04:05.000")
	}
	bytes := func(n *int64) string {
		if n == nil {
			return ""
		}
		return fmt.Sprint(*n)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		e := rows[i]
		ssl := "—"
		if e.Decrypted {
			ssl = "已解密"
		} else if e.Scope == "tunnel" || strings.HasPrefix(e.URL, "https:") {
			ssl = "加密"
		}
		code := ""
		if e.StatusCode > 0 {
			code = strconv.Itoa(e.StatusCode)
		}
		if err := cw.Write([]string{fmt.Sprint(e.ID), e.URL, e.Device, e.Serial, e.App, e.Method, code, e.Status, clock(e.Started), clock(e.Ended), fmt.Sprint(e.DurationMS), bytes(e.RequestBytes), bytes(e.ResponseBytes), ssl, e.ContentType, fmt.Sprint(e.Shaped), e.Detail}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func (a *App) requestHistoryRoutes(mux *http.ServeMux) {
	filter := func(r *http.Request) (RequestHistoryFilter, error) {
		q := r.URL.Query()
		f := RequestHistoryFilter{Serial: q.Get("serial"), App: q.Get("app"), Host: q.Get("host"), Status: q.Get("status"), Type: q.Get("type"), Query: q.Get("q"), Shaped: q.Get("shaped")}
		var err error
		if q.Get("since") != "" {
			f.Since, err = strconv.ParseInt(q.Get("since"), 10, 64)
			if err != nil {
				return f, fmt.Errorf("开始时间范围不正确")
			}
		}
		if q.Get("until") != "" {
			f.Until, err = strconv.ParseInt(q.Get("until"), 10, 64)
			if err != nil {
				return f, fmt.Errorf("开始时间范围不正确")
			}
		}
		if f.Since < 0 || f.Until < 0 || f.Since > 0 && f.Until > 0 && f.Since > f.Until {
			return f, fmt.Errorf("开始时间范围不正确")
		}
		if f.Shaped != "true" && f.Shaped != "false" {
			f.Shaped = ""
		}
		return f, nil
	}
	mux.HandleFunc("GET /api/request-history", func(w http.ResponseWriter, r *http.Request) {
		f, err := filter(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 1000 {
			limit = 100
		}
		before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
		rows, summary, next := a.queryRequestHistory(f, limit, before)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"rows": rows, "summary": summary, "next_before": next})
	})
	mux.HandleFunc("GET /api/request-history.csv", func(w http.ResponseWriter, r *http.Request) {
		f, err := filter(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=weaknet-requests-%s.csv", time.Now().Format("20060102-150405")))
		w.Write([]byte("\xEF\xBB\xBF"))
		a.writeRequestHistoryCSV(w, f)
	})
	mux.HandleFunc("POST /api/request-history/clear", func(w http.ResponseWriter, r *http.Request) {
		a.clearRequestData()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
}
