package main

import (
	"encoding/json"
	"net/http"
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
		var s HTTPSSettings
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&s); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := a.https.update(s); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.event("HTTPS 解密设置已保存，对新连接生效")
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
