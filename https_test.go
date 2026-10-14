package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 测试不依赖开发者本地放入 embedded_ca/ 的共享 CA；需要内置 CA 的测试自行设置。
func init() { embeddedCA = nil }

func TestEmbeddedCASharedAcrossMachines(t *testing.T) {
	source, err := loadLocalCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	embeddedCA = source
	defer func() { embeddedCA = nil }()
	first, second := t.TempDir(), t.TempDir()
	a, err := openHTTPSManager(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := openHTTPSManager(second)
	if err != nil {
		t.Fatal(err)
	}
	if a.status().Fingerprint != b.status().Fingerprint || a.status().Source != "程序内置" {
		t.Fatalf("machines should share the embedded CA: %+v %+v", a.status(), b.status())
	}
	if _, err := os.Stat(filepath.Join(first, "ca.pem")); !os.IsNotExist(err) {
		t.Fatal("embedded CA must not generate or write a local identity")
	}
	if err = a.update(HTTPSSettings{Enabled: true, Domains: []string{"api.example.com"}}); err != nil {
		t.Fatal(err)
	}
	embeddedCA = []byte("broken")
	if _, err = openHTTPSManager(t.TempDir()); err == nil || !strings.Contains(err.Error(), "程序内置的 CA") {
		t.Fatalf("invalid embedded CA should fail clearly, got %v", err)
	}
}

func testHTTPS(t *testing.T, app *App) *HTTPSManager {
	t.Helper()
	m, err := openHTTPSManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.update(HTTPSSettings{Enabled: true, Domains: []string{"127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	app.https = m
	return m
}
func TestFixedCAPersistenceAndPublicExport(t *testing.T) {
	dir := t.TempDir()
	m, err := openHTTPSManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	settings := HTTPSSettings{true, []string{"API.Example.com.", "api.example.com", "127.0.0.1"}}
	if err = m.update(settings); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	again, err := openHTTPSManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if !bytes.Equal(before, after) || m.status().Fingerprint != again.status().Fingerprint {
		t.Fatal("CA changed across restart")
	}
	if !again.enabledFor("sub.api.example.com") || again.enabledFor("evilapi.example.com") || !again.enabledFor("API.EXAMPLE.COM.") {
		t.Fatal("domain selection not restored")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dir, "ca.pem"))
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("private key mode %v", fi.Mode())
		}
	}
	app := newTestApp(t, "unlimited")
	app.https = again
	for _, path := range []string{"/api/state", "/api/https", "/api/https/ca.crt", "/api/https/android.xml"} {
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE KEY") {
			t.Fatalf("unsafe export %s %d", path, w.Code)
		}
		if path == "/api/https/ca.crt" {
			block, rest := pem.Decode(w.Body.Bytes())
			if block == nil || len(rest) > 0 {
				t.Fatal("expected public certificate only")
			}
			sum := sha256.Sum256(block.Bytes)
			if strings.ToUpper(hex.EncodeToString(sum[:])) != m.status().Fingerprint {
				t.Fatal("fingerprint mismatch")
			}
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = openHTTPSManager(dir); err == nil {
		t.Fatal("silently replaced invalid fixed CA")
	}
	broken, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if string(broken) != "broken" {
		t.Fatal("overwrote broken identity")
	}
}
func TestConcurrentCAInitialization(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan *HTTPSManager, 8)
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m, e := openHTTPSManager(dir); results <- m; errors <- e }()
	}
	wg.Wait()
	close(results)
	close(errors)
	for e := range errors {
		if e != nil {
			t.Fatal(e)
		}
	}
	fingerprint := ""
	for m := range results {
		f := m.status().Fingerprint
		if fingerprint != "" && fingerprint != f {
			t.Fatal("concurrent launches use different CAs")
		}
		fingerprint = f
	}
}
func TestHTTPSSettingsRejectInvalidAndKeepPrevious(t *testing.T) {
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	for _, domain := range []string{"https://api.example", "api.example:443", "*.example.com", "a/b", "-bad.example", strings.Repeat("a", 254)} {
		body, _ := json.Marshal(HTTPSSettings{true, []string{domain}})
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, httptest.NewRequest("POST", "/api/https", bytes.NewReader(body)))
		if w.Code != 400 || !m.enabledFor("127.0.0.1") {
			t.Fatalf("invalid setting changed config: %q %d", domain, w.Code)
		}
	}
	if err := m.update(HTTPSSettings{Enabled: true}); err == nil {
		t.Fatal("enabled all domains without opt-in")
	}
	if err := m.update(HTTPSSettings{}); err != nil || m.enabledFor("127.0.0.1") {
		t.Fatalf("disable failed %v", err)
	}
}
func tlsProxyClient(lane *Lane, roots *x509.CertPool) (*http.Client, *http.Transport) {
	proxy, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", lane.port))
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{RootCAs: roots}}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, tr
}
func TestHTTPSDecryptionKeepAliveAndStatuses(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprint(bypass), func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			if bypass {
				app.cfg.Update(map[string]any{"whitelist": "127.0.0.1"})
			}
			m := testHTTPS(t, app)
			var upstreamConnections int
			var mu sync.Mutex
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/bad" {
					w.WriteHeader(503)
				}
				if r.URL.Query().Get("token") != "secret-value" {
					t.Error("request query changed")
				}
				w.Write([]byte(r.URL.Path))
			}))
			srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
				if s == http.StateNew {
					mu.Lock()
					upstreamConnections++
					mu.Unlock()
				}
			}
			srv.StartTLS()
			defer srv.Close()
			m.upstreamRoots = x509.NewCertPool()
			m.upstreamRoots.AddCert(srv.Certificate())
			roots := x509.NewCertPool()
			roots.AddCert(m.ca.root)
			lane := startLane(t, app, "phone")
			client, tr := tlsProxyClient(lane, roots)
			defer tr.CloseIdleConnections()
			for i, path := range []string{"/ok", "/bad"} {
				resp, err := client.Get(srv.URL + path + "?token=secret-value")
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(body) != path {
					t.Fatalf("body %q %v", body, err)
				}
				if resp.TLS.PeerCertificates[0].Issuer.CommonName != m.ca.root.Subject.CommonName {
					t.Fatal("not decrypted")
				}
				status := "complete"
				if i == 1 {
					status = "failed"
				}
				waitEventStatus(t, app, uint64(i+1), status)
			}
			mu.Lock()
			n := upstreamConnections
			mu.Unlock()
			if n != 1 {
				t.Fatalf("upstream keep-alive lost: %d", n)
			}
			events := app.state("").EventDetails
			if len(events) != 2 {
				t.Fatalf("expected one event per HTTP request: %+v", events)
			}
			for _, e := range events {
				if strings.Contains(e.Text, "secret-value") || !strings.Contains(e.Text, "token=REDACTED") || e.Scope != "http" {
					t.Fatalf("unsafe/wrong event %+v", e)
				}
			}
			rows, _ := app.conns.query(ConnFilter{}, 10)
			if len(rows) != 1 || rows[0].Status != "active" || rows[0].Shaped == bypass || rows[0].Down == 0 {
				t.Fatalf("bad tunnel accounting %+v", rows)
			}
		})
	}
}
func TestHTTPSUpstreamValidationAndClientTrust(t *testing.T) {
	for _, trustClient := range []bool{true, false} {
		t.Run(fmt.Sprint(trustClient), func(t *testing.T) {
			app := newTestApp(t, "unlimited")
			m := testHTTPS(t, app)
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted request reached origin") }))
			defer srv.Close()
			roots := x509.NewCertPool()
			if trustClient {
				roots.AddCert(m.ca.root)
			}
			lane := startLane(t, app, "phone")
			client, tr := tlsProxyClient(lane, roots)
			defer tr.CloseIdleConnections()
			resp, err := client.Get(srv.URL + "/request")
			if trustClient {
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 502 {
					t.Fatalf("upstream certificate validation skipped: %d", resp.StatusCode)
				}
			} else if err == nil {
				resp.Body.Close()
				t.Fatal("client did not require CA trust")
			}
			waitEventStatus(t, app, 1, "failed")
		})
	}
}
func TestUnselectedHTTPSStaysTunnel(t *testing.T) {
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	m.update(HTTPSSettings{true, []string{"other.example"}})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("original")) }))
	defer srv.Close()
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	lane := startLane(t, app, "phone")
	client, tr := tlsProxyClient(lane, roots)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	tr.CloseIdleConnections()
	waitEventStatus(t, app, 1, "complete")
	e := app.state("").EventDetails[0]
	if e.Scope != "tunnel" {
		t.Fatalf("unexpected decryption %+v", e)
	}
}
func TestHTTPSShapingAndReset(t *testing.T) {
	app := newTestApp(t, "unlimited")
	app.cfg.Update(map[string]any{"down_kbps": 512, "delay_ms": 5})
	m := testHTTPS(t, app)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte("x"), 64<<10)) }))
	defer srv.Close()
	m.upstreamRoots = x509.NewCertPool()
	m.upstreamRoots.AddCert(srv.Certificate())
	roots := x509.NewCertPool()
	roots.AddCert(m.ca.root)
	lane := startLane(t, app, "phone")
	client, tr := tlsProxyClient(lane, roots)
	defer tr.CloseIdleConnections()
	start := time.Now()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || n != 64<<10 || time.Since(start) < 700*time.Millisecond {
		t.Fatalf("shaping bypassed: %d %v %v", n, err, time.Since(start))
	}
	waitEventStatus(t, app, 1, "complete")
	lane.resetConns(true)
	deadline := time.Now().Add(time.Second)
	for {
		lane.mu.Lock()
		n := len(lane.active)
		lane.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TLS relay not cleaned up after reset")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestHTTPSWebSocketUpgrade(t *testing.T) {
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		rw.Flush()
		buf := make([]byte, 4)
		if _, err = io.ReadFull(rw, buf); err == nil {
			rw.Write(buf)
			rw.Flush()
		}
	}))
	defer srv.Close()
	m.upstreamRoots = x509.NewCertPool()
	m.upstreamRoots.AddCert(srv.Certificate())
	lane := startLane(t, app, "phone")
	raw, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lane.port))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	target := strings.TrimPrefix(srv.URL, "https://")
	fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(raw)
	if _, err = readHead(br); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(m.ca.root)
	secure := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"})
	defer secure.Close()
	fmt.Fprintf(secure, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", target)
	reader := bufio.NewReader(secure)
	head, err := readHead(reader)
	if err != nil || !strings.Contains(head, "101") {
		t.Fatalf("upgrade %q %v", head, err)
	}
	secure.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err = io.ReadFull(reader, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("upgrade stream %q %v", buf, err)
	}
	secure.Close()
	waitEventStatus(t, app, 1, "complete")
}

func TestHTTPSHTTP2UpstreamAndTruncatedResponse(t *testing.T) {
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	protocols := make(chan int, 2)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocols <- r.ProtoMajor
		if r.Header.Get("Accept-Encoding") != "" {
			t.Error("proxy inserted compression header")
		}
		if r.URL.Path == "/truncated" {
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("short"))
			return
		}
		w.Write([]byte("ok"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	m.upstreamRoots = x509.NewCertPool()
	m.upstreamRoots.AddCert(srv.Certificate())
	roots := x509.NewCertPool()
	roots.AddCert(m.ca.root)
	lane := startLane(t, app, "phone")
	client, tr := tlsProxyClient(lane, roots)
	tr.DisableCompression = true
	defer tr.CloseIdleConnections()
	for i, path := range []string{"/ok", "/truncated"} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			if i == 1 {
				waitEventStatus(t, app, 2, "failed")
				continue
			}
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if i == 1 && err == nil {
			t.Fatal("truncated response accepted")
		}
		status := "complete"
		if i == 1 {
			status = "failed"
		}
		waitEventStatus(t, app, uint64(i+1), status)
	}
	for i := 0; i < 2; i++ {
		if p := <-protocols; p != 2 {
			t.Fatalf("expected HTTP/2 upstream, got %d", p)
		}
	}
}

func TestHTTPSRejectsDifferentHost(t *testing.T) {
	app := newTestApp(t, "unlimited")
	m := testHTTPS(t, app)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("mismatched request forwarded") }))
	defer srv.Close()
	m.upstreamRoots = x509.NewCertPool()
	m.upstreamRoots.AddCert(srv.Certificate())
	roots := x509.NewCertPool()
	roots.AddCert(m.ca.root)
	lane := startLane(t, app, "phone")
	client, tr := tlsProxyClient(lane, roots)
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Host = "other.example"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 421 {
		t.Fatalf("mismatched Host permitted: %d", resp.StatusCode)
	}
}

func rsaCAIdentity(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "Partner Debug CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})...)
}

func TestImportExportAndRemoveCA(t *testing.T) {
	dir := t.TempDir()
	m, err := openHTTPSManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := m.status()
	if err = m.importCA([]byte("broken")); err == nil || m.status().Fingerprint != original.Fingerprint {
		t.Fatal("invalid import must fail and keep the current CA")
	}
	if err = m.importCA(rsaCAIdentity(t)); err != nil {
		t.Fatal(err)
	}
	imported := m.status()
	if imported.Source != caSourceImported || imported.Subject != "Partner Debug CA" || imported.Fingerprint == original.Fingerprint {
		t.Fatalf("import not applied: %+v", imported)
	}
	leaf, err := m.certificate("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = leaf.Leaf.CheckSignatureFrom(m.ca.root); err != nil || m.ca.root.Subject.CommonName != "Partner Debug CA" {
		t.Fatalf("leaf must be issued by the imported CA: %v", err)
	}
	exported, err := m.identity()
	if err != nil {
		t.Fatal(err)
	}
	again, err := openHTTPSManager(dir)
	if err != nil || again.status().Fingerprint != imported.Fingerprint {
		t.Fatalf("imported CA must persist across restarts: %v", err)
	}
	other, err := openHTTPSManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = other.importCA(exported); err != nil || other.status().Fingerprint != imported.Fingerprint {
		t.Fatalf("exported identity must import elsewhere: %v", err)
	}
	if err = m.removeImportedCA(); err != nil {
		t.Fatal(err)
	}
	if st := m.status(); st.Source != caSourceLocal || st.Fingerprint != original.Fingerprint {
		t.Fatalf("remove should restore the original CA: %+v", st)
	}
	if _, err = os.Stat(filepath.Join(dir, importedCAFile)); !os.IsNotExist(err) {
		t.Fatal("imported identity file should be removed")
	}
	if err = m.removeImportedCA(); err == nil {
		t.Fatal("removing without an imported CA should fail")
	}
}

func TestCAEndpointsOnlyServeLocalConsole(t *testing.T) {
	app := newTestApp(t, "unlimited")
	var err error
	if app.https, err = openHTTPSManager(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	routes := app.routes()
	do := func(method, target, host string, header map[string]string, body string) int {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = host
		for k, v := range header {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)
		return w.Code
	}
	if code := do("GET", "/api/https/ca/identity.pem", "evil.example:8899", nil, ""); code != 403 {
		t.Fatalf("DNS rebinding host should be rejected, got %d", code)
	}
	if code := do("GET", "/api/https/ca/identity.pem", "127.0.0.1:8899", map[string]string{"Sec-Fetch-Site": "cross-site"}, ""); code != 403 {
		t.Fatalf("cross-site export should be rejected, got %d", code)
	}
	if code := do("GET", "/api/https/ca/identity.pem", "127.0.0.1:8899", map[string]string{"Sec-Fetch-Site": "same-origin"}, ""); code != 200 {
		t.Fatalf("local export should work, got %d", code)
	}
	body, _ := json.Marshal(map[string]string{"pem": string(rsaCAIdentity(t))})
	if code := do("POST", "/api/https/ca/import", "127.0.0.1:8899", map[string]string{"Content-Type": "text/plain"}, string(body)); code != 403 {
		t.Fatalf("non-JSON import should be rejected, got %d", code)
	}
	if code := do("POST", "/api/https/ca/import", "127.0.0.1:8899", map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, string(body)); code != 403 {
		t.Fatalf("foreign origin import should be rejected, got %d", code)
	}
	if app.https.status().Source == caSourceImported {
		t.Fatal("rejected requests must not change the CA")
	}
	if code := do("POST", "/api/https/ca/import", "127.0.0.1:8899", map[string]string{"Content-Type": "application/json", "Origin": "http://127.0.0.1:8899"}, string(body)); code != 200 || app.https.status().Source != caSourceImported {
		t.Fatalf("local import should work, got %d", code)
	}
	if code := do("POST", "/api/https/ca/reset", "localhost:8899", map[string]string{"Content-Type": "application/json"}, "{}"); code != 200 || app.https.status().Source != caSourceLocal {
		t.Fatalf("local reset should work, got %d", code)
	}
}
