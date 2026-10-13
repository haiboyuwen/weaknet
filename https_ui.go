package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (a *App) httpsRoutes(mux *http.ServeMux) {
	ready := func(w http.ResponseWriter) bool {
		if a.https == nil {
			http.Error(w, "固定 CA 尚未初始化", http.StatusServiceUnavailable)
			return false
		}
		return true
	}
	status := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(a.https.status())
	}
	mux.HandleFunc("GET /api/https", func(w http.ResponseWriter, r *http.Request) {
		if ready(w) {
			status(w)
		}
	})
	mux.HandleFunc("POST /api/https", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		var input struct {
			HTTPSSettings
			Reconnect bool `json:"reconnect"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		previous := a.https.status().HTTPSSettings
		if err := a.https.update(input.HTTPSSettings); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		count := 0
		if input.Reconnect {
			count = a.reconnectHTTPS(previous, a.https.status().HTTPSSettings)
		}
		a.event(fmt.Sprintf("HTTPS 解密设置已保存，重连 %d 条受影响的 HTTPS 连接", count))
		status(w)
	})
	mux.HandleFunc("GET /api/https/ca.crt", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="weaknet_ca.crt"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Write(a.https.public)
	})
	mux.HandleFunc("GET /api/https/android.xml", func(w http.ResponseWriter, r *http.Request) {
		if !ready(w) {
			return
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="network_security_config.xml"`)
		w.Write([]byte(androidDebugConfig))
	})
}

func (a *App) reconnectHTTPS(before, after HTTPSSettings) int {
	applies := func(s HTTPSSettings, host string) bool {
		return s.Enabled && matchDomain(strings.ToLower(strings.TrimSuffix(host, ".")), s.Domains)
	}
	a.mu.Lock()
	lanes := make([]*Lane, 0, len(a.lanes))
	for _, l := range a.lanes {
		lanes = append(lanes, l)
	}
	a.mu.Unlock()
	count := 0
	for _, l := range lanes {
		count += l.resetMatching(func(c *connPair) bool {
			return c.tunnel && c.record != nil && applies(before, c.record.Host) != applies(after, c.record.Host)
		}, "HTTPS 解密范围变化，重新建立连接")
	}
	return count
}
