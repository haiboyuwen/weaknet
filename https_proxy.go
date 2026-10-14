package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"
)

type singleConnListener struct {
	conn     net.Conn
	accepted bool
	closed   chan struct{}
	once     sync.Once
}

func (s *singleConnListener) Accept() (net.Conn, error) {
	if !s.accepted {
		s.accepted = true
		return s.conn, nil
	}
	<-s.closed
	return nil, net.ErrClosed
}
func (s *singleConnListener) Close() error   { s.once.Do(func() { close(s.closed) }); return nil }
func (s *singleConnListener) Addr() net.Addr { return s.conn.LocalAddr() }

func (l *Lane) handleHTTPS(client net.Conn, br *bufio.Reader, host, port, app string, shaped bool) {
	rec := l.app.conns.open(l, app, host, port, shaped)
	defer rec.finish("ok", "")
	failure := func(err error) { rec.finish("failed", err.Error()) }
	if shaped && l.cfg.Get().Outage == "reset" {
		rec.finish("rejected", "断开模式")
		client.Close()
		return
	}
	if app == "" {
		app = "未识别 App"
	}
	cert, err := l.app.https.certificate(host)
	if err != nil {
		failure(err)
		client.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		client.Close()
		return
	}
	decrypted, bridge := net.Pipe()
	secure := tls.Server(decrypted, &tls.Config{Certificates: []tls.Certificate{*cert}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
	listener := &singleConnListener{conn: secure, closed: make(chan struct{})}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Requests inside this tunnel may only reach its original CONNECT destination.
			return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(l.app.resolve(host), port))
		},
		TLSClientConfig:    &tls.Config{ServerName: host, RootCAs: l.app.https.upstreamRoots, MinVersion: tls.VersionTLS12},
		DisableCompression: true, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout: 90 * time.Second,
	}
	server := &http.Server{Handler: l.httpsHandler(host, port, app, shaped, transport), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, BaseContext: func(net.Listener) context.Context { return l.ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer secure.Close()
		ctx, cancel := context.WithTimeout(l.ctx, 30*time.Second)
		err := secure.HandshakeContext(ctx)
		cancel()
		if err != nil {
			event := l.app.recordEvent(EventView{Method: "CONNECT", URL: "https://" + net.JoinHostPort(host, port), Text: fmt.Sprintf("[%s] [HTTP][代理] CONNECT %s · %s · HTTPS 解密握手失败", l.name, net.JoinHostPort(host, port), app), Status: "active", Scope: "http", Serial: l.serial, Device: l.name, App: app, Host: host, Shaped: shaped})
			l.app.failEvent(event, fmt.Errorf("%w；请确认调试包信任固定 CA，且未启用自定义证书固定", err))
			failure(err)
			return
		}
		server.Serve(listener)
	}()
	defer func() {
		bridge.Close()
		secure.Close()
		listener.Close()
		server.Close()
		transport.CloseIdleConnections()
		<-done
	}()
	key := statKey(app, host)
	if shaped {
		l.relayShaped(client, br, bridge, key, "CONNECT", nil, rec, failure, host, app)
	} else {
		l.bypass(client, br, bridge, key, "CONNECT", nil, rec, failure)
	}
}

func sameHTTPSAuthority(authority, host, port string) bool {
	h, p, err := net.SplitHostPort(authority)
	if err != nil {
		h = strings.Trim(authority, "[]")
		p = "443"
	}
	return strings.EqualFold(strings.TrimSuffix(h, "."), strings.TrimSuffix(host, ".")) && p == port
}

func (l *Lane) httpsHandler(host, port, app string, shaped bool, transport http.RoundTripper) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameHTTPSAuthority(r.Host, host, port) {
			http.Error(w, "请求 Host 必须与 CONNECT 目标一致", http.StatusMisdirectedRequest)
			return
		}
		target := net.JoinHostPort(host, port)
		mode := "直连"
		if shaped {
			mode = "弱网"
		}
		id := l.requestEvent("代理", app, r.Method, "https://"+target+r.URL.RequestURI(), " · HTTPS 解密 · "+mode)
		address, _ := displayURL("https://" + target + r.URL.RequestURI())
		l.app.inspections.begin(id, l, app, r.Method, address, r)
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &inspectionBody{ReadCloser: r.Body, store: &l.app.inspections, id: id}
		}
		start := time.Now()
		status := 0
		failed := false
		completed := false

		var finalErr error
		defer func() {
			result, detail := "complete", fmt.Sprintf("HTTP %d · %s", status, time.Since(start).Round(time.Millisecond))
			if failed {
				result = "failed"
				if finalErr != nil {
					detail = finalErr.Error()
					var timeout net.Error
					if errors.As(finalErr, &timeout) && timeout.Timeout() {
						result = "timeout"
					}
				}
			} else if !completed {
				result = "failed"
				detail = "响应传输中断"
			} else if status >= 400 {
				result = "failed"
			}
			l.app.finishEvent(id, result, detail)
			l.app.inspections.finish(id, result, detail)
		}()

		proxy := &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "https"
				pr.Out.URL.Host = target
				pr.Out.Host = target
			},
			ModifyResponse: func(resp *http.Response) error {
				status = resp.StatusCode
				l.app.inspections.response(id, resp)
				if status != http.StatusSwitchingProtocols && resp.Body != nil && resp.Body != http.NoBody {
					resp.Body = &inspectionBody{ReadCloser: resp.Body, store: &l.app.inspections, id: id, response: true}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				failed = true
				finalErr = err
				l.app.failEvent(id, err)
				http.Error(w, "HTTPS 上游请求失败", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
		if !failed && status != http.StatusSwitchingProtocols {
			if err := http.NewResponseController(w).Flush(); err != nil {
				failed = true
				finalErr = err
				l.app.failEvent(id, err)
			}
		}
		completed = true
	})
}
