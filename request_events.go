package main

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// displayURL 只保留排查所需的地址：不记录用户信息、fragment 或查询参数值。
func displayURL(raw string) (string, bool) {
	if len(raw) > 8192 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	if u.RawQuery != "" {
		params := strings.Split(u.RawQuery, "&")
		for i, p := range params {
			key, _, hasValue := strings.Cut(p, "=")
			if hasValue && key != "" {
				params[i] = key + "=REDACTED"
			} else {
				params[i] = "REDACTED"
			}
		}
		u.RawQuery = strings.Join(params, "&")
	}
	return u.String(), true
}

func requestKind(raw string) string {
	u, err := url.Parse(raw)
	if err == nil {
		switch strings.ToLower(path.Ext(u.Path)) {
		case ".mp4", ".svga", ".gif", ".webp", ".png", ".jpg", ".jpeg", ".avif",
			".zip", ".mp3", ".wav", ".ogg", ".aac", ".webm", ".ttf", ".otf", ".woff", ".woff2":
			return "资源"
		}
	}
	return "HTTP"
}

func (l *Lane) requestEvent(source, app, method, raw, detail string) {
	address, ok := displayURL(raw)
	if !ok {
		return
	}
	if app == "" {
		app = "未识别 App"
	}
	if method != "" {
		address = method + " " + address
	}
	l.event(fmt.Sprintf("[%s][%s] %s · %s%s", requestKind(raw), source, address, app, detail))
}

func (l *Lane) proxyRequestEvent(app, method, target string, shaped bool) {
	mode := "直连"
	if shaped {
		mode = "弱网"
	}
	if method == "CONNECT" {
		if app == "" {
			app = "未识别 App"
		}
		l.event(fmt.Sprintf("[HTTPS 隧道][代理] CONNECT %s · %s · %s（加密路径不可见）", target, app, mode))
		return
	}
	l.requestEvent("代理", app, method, target, " · "+mode)
}
