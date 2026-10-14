package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// localConsoleRequest 只放行本机控制台页面自己发起的请求，用于导入导出 CA 私钥等敏感操作：
// 校验 Host 防 DNS 重绑定，校验 Origin / Sec-Fetch-Site 防其他网页借浏览器调用；
// 写操作要求 JSON，跨站请求会先触发 CORS 预检而被浏览器拦下。
func localConsoleRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
			return false
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	return r.Method == http.MethodGet || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

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
		w.Write(a.https.publicPEM())
	})
	mux.HandleFunc("GET /api/https/android-user.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="network_security_config.xml"`)
		w.Write([]byte(androidUserConfig))
	})
	sensitive := func(w http.ResponseWriter, r *http.Request) bool {
		if !ready(w) {
			return false
		}
		if !localConsoleRequest(r) {
			http.Error(w, "仅允许本机控制台页面操作 CA", http.StatusForbidden)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/https/ca/identity.pem", func(w http.ResponseWriter, r *http.Request) {
		if !sensitive(w, r) {
			return
		}
		identity, err := a.https.identity()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.event("已导出 CA 身份（含私钥）")
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="weaknet_ca_identity.pem"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Write(identity)
	})
	mux.HandleFunc("POST /api/https/ca/import", func(w http.ResponseWriter, r *http.Request) {
		if !sensitive(w, r) {
			return
		}
		var input struct {
			PEM string `json:"pem"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := a.https.importCA([]byte(input.PEM)); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		st := a.https.status()
		a.event(fmt.Sprintf("已导入 CA %s（SHA-256 %s…），重连 %d 条已解密的 HTTPS 连接", st.Subject, st.Fingerprint[:16], a.reconnectDecrypted()))
		status(w)
	})
	mux.HandleFunc("POST /api/https/ca/reset", func(w http.ResponseWriter, r *http.Request) {
		if !sensitive(w, r) {
			return
		}
		if err := a.https.removeImportedCA(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.event(fmt.Sprintf("已移除导入的 CA，恢复为%s的 CA，重连 %d 条已解密的 HTTPS 连接", a.https.status().Source, a.reconnectDecrypted()))
		status(w)
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

// reconnectDecrypted 更换 CA 后断开已解密的连接，让 App 重连时拿到新 CA 签发的证书。
func (a *App) reconnectDecrypted() int {
	current := a.https.status().HTTPSSettings
	return a.reconnectHTTPS(current, HTTPSSettings{})
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
