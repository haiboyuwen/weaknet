package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed page.html
var pageHTML []byte

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(pageHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.state(r.URL.Query().Get("app")))
	})
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if a.cfg.Update(data) {
			a.resetShaped()
		}
		b, _ := json.Marshal(data)
		a.event("参数 " + string(b))
		writeJSON(w, a.cfg.Get())
	})
	mux.HandleFunc("POST /api/preset/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !a.cfg.ApplyPreset(name) {
			http.Error(w, "unknown preset", http.StatusNotFound)
			return
		}
		a.event("切换档位 " + findPreset(name).Label)
		writeJSON(w, a.cfg.Get())
	})
	mux.HandleFunc("POST /api/device/{serial}/clear", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Pkg    string `json:"pkg"`
			Launch bool   `json:"launch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.clearAppData(r.PathValue("serial"), strings.TrimSpace(req.Pkg), req.Launch); err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/device/{serial}/{action}", func(w http.ResponseWriter, r *http.Request) {
		a.deviceAction(r.PathValue("serial"), r.PathValue("action"))
		writeJSON(w, map[string]bool{"ok": true})
	})
	connFilter := func(r *http.Request) ConnFilter {
		q := r.URL.Query()
		return ConnFilter{Serial: q.Get("serial"), App: q.Get("app"), Host: q.Get("host"), Status: q.Get("status")}
	}
	mux.HandleFunc("GET /api/conns", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2000 {
			limit = 300
		}
		rows, sum := a.conns.query(connFilter(r), limit)
		writeJSON(w, map[string]any{"rows": rows, "summary": sum})
	})
	mux.HandleFunc("GET /api/conns.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=weaknet-conns-%s.csv", time.Now().Format("20060102-150405")))
		w.Write([]byte("\xEF\xBB\xBF")) // BOM，Excel 打开中文不乱码
		a.conns.writeCSV(w, connFilter(r))
	})
	mux.HandleFunc("POST /api/conns/clear", func(w http.ResponseWriter, r *http.Request) {
		a.conns.clear()
		a.event("已清空连接历史")
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		a.event("收到退出请求")
		go a.requestQuit()
	})
	return mux
}

func (a *App) serveUI(port int) error {
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: a.routes()}
	return srv.ListenAndServe()
}
