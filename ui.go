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
	a.httpsRoutes(mux)
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
		if err := validateConfigInput(data); err != nil {
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
		if err := a.deviceAction(r.PathValue("serial"), r.PathValue("action")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	connFilter := func(r *http.Request) ConnFilter {
		q := r.URL.Query()
		since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		until, _ := strconv.ParseInt(q.Get("until"), 10, 64)
		shaped := q.Get("shaped")
		if shaped != "true" && shaped != "false" {
			shaped = ""
		}
		return ConnFilter{Serial: q.Get("serial"), App: q.Get("app"), Host: q.Get("host"), Status: q.Get("status"),
			Shaped: shaped, Since: since, Until: until}
	}
	mux.HandleFunc("GET /api/conns", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2000 {
			limit = 300
		}
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		rows, sum, next := a.conns.queryPage(connFilter(r), limit, before)
		writeJSON(w, map[string]any{"rows": rows, "summary": sum, "next_before": next})
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
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		level := q.Get("level")
		if level != "" && (len(level) != 1 || !strings.Contains("VDIWEF", level)) {
			http.Error(w, "日志级别不合法", http.StatusBadRequest)
			return
		}
		writeJSON(w, a.adbLogs.snapshot(ADBLogFilter{Serial: q.Get("serial"), App: q.Get("app"), Level: level, Query: q.Get("q")}))
	})
	mux.HandleFunc("POST /api/logs/clear", func(w http.ResponseWriter, r *http.Request) {
		a.adbLogs.clear(r.URL.Query().Get("serial"))
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logs/{serial}/start", func(w http.ResponseWriter, r *http.Request) {
		if err := a.startADBLogs(r.PathValue("serial")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logs/{serial}/stop", func(w http.ResponseWriter, r *http.Request) {
		a.stopADBLogs(r.PathValue("serial"))
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		a.event("收到退出请求")
		go a.requestQuit()
	})
	return mux
}

// 在修改任何字段前验证整次提交，避免方案部分应用后才发现参数非法。
func validateConfigInput(data map[string]any) error {
	for _, key := range []string{"down_kbps", "up_kbps", "delay_ms", "jitter_ms", "loss_pct", "window_kb"} {
		if raw, exists := data[key]; exists {
			n, ok := raw.(float64)
			if !ok || n < 0 || (key == "loss_pct" && n > 100) || (key == "window_kb" && n < 1) {
				return fmt.Errorf("参数 %s 不合法", key)
			}
		}
	}
	if raw, exists := data["outage"]; exists {
		if s, ok := raw.(string); !ok || (s != "" && s != "stall" && s != "reset") {
			return fmt.Errorf("网络状态不合法")
		}
	}
	if raw, exists := data["blacklist_on"]; exists {
		if _, ok := raw.(bool); !ok {
			return fmt.Errorf("黑名单开关必须为布尔值")
		}
	}
	for _, key := range []string{"blacklist", "whitelist"} {
		if raw, exists := data[key]; exists {
			switch list := raw.(type) {
			case string:
			case []any:
				for _, item := range list {
					if _, ok := item.(string); !ok {
						return fmt.Errorf("%s 名单项必须为字符串", key)
					}
				}
			default:
				return fmt.Errorf("%s 必须为字符串或字符串数组", key)
			}
		}
	}
	return nil
}

func (a *App) serveUI(port int) error {
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: a.routes()}
	return srv.ListenAndServe()
}
