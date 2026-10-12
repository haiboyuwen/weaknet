package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestADBLogParsingFilteringAndBounds(t *testing.T) {
	row, ok := parseADBLog("10-12 00:01:02.123  123  456 E AndroidRuntime: java.net.SocketTimeoutException: timeout <script>")
	if !ok || row.PID != "123" || row.TID != "456" || row.Level != "E" || row.Tag != "AndroidRuntime" || !strings.Contains(row.Message, "<script>") {
		t.Fatalf("bad parse: %+v", row)
	}
	if _, ok := parseADBLog("--------- beginning of crash"); ok {
		t.Fatal("buffer divider should not be a log")
	}
	big, _ := parseADBLog("10-12 00:01:02.123 123 456 D Test: " + strings.Repeat("a", 20000))
	if len(big.Message) > 8250 {
		t.Fatal("log message unbounded")
	}
	var store adbLogStore
	for i := 0; i < adbLogLimit+3; i++ {
		r := row
		r.Serial = "one"
		r.App = "com.example:worker"
		r.Message = fmt.Sprintf("timeout %d", i)
		if i%2 == 0 {
			r.Serial = "two"
			r.Level = "I"
		}
		store.append(r)
	}
	all := store.snapshot(ADBLogFilter{})
	if len(all.Rows) != adbLogLimit || all.Rows[0].ID != adbLogLimit+3 {
		t.Fatal("retention/order incorrect")
	}
	filtered := store.snapshot(ADBLogFilter{Serial: "one", App: "com.example", Level: "W", Query: "TIMEOUT"})
	if len(filtered.Rows) == 0 {
		t.Fatal("subprocess/priority/query filter failed")
	}
	for _, r := range filtered.Rows {
		if r.Serial != "one" || r.Level != "E" {
			t.Fatal("bad filter")
		}
	}
	filtered.Rows[0].Message = "mutated"
	if store.snapshot(ADBLogFilter{}).Rows[0].Message == "mutated" {
		t.Fatal("snapshot shares backing memory")
	}
	store.clear("one")
	for _, r := range store.snapshot(ADBLogFilter{}).Rows {
		if r.Serial == "one" {
			t.Fatal("scoped clear failed")
		}
	}
	store.clear("")
	if len(store.snapshot(ADBLogFilter{}).Rows) != 0 {
		t.Fatal("clear failed")
	}
}
func fakeLogADB(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake adb fixture")
	}
	path := filepath.Join(t.TempDir(), "fake adb")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
func waitADBLogState(t *testing.T, a *App, serial, status string) ADBLogSnapshot {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := a.adbLogs.snapshot(ADBLogFilter{})
		for _, s := range snapshot.Sessions {
			if s.Serial == serial && s.Status == status {
				return snapshot
			}
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatalf("session %s did not reach %s: %+v", serial, status, a.adbLogs.snapshot(ADBLogFilter{}))
	return ADBLogSnapshot{}
}
func TestADBLogCaptureIndependentLifecycle(t *testing.T) {
	a := newTestApp(t, "weak")
	a.adb = fakeLogADB(t, `case "$*" in
 *"shell ps"*) printf 'PID NAME\n123 com.example:worker\n'; exit 0;;
 *"logcat"*) printf '10-12 00:01:02.123 123 456 E AndroidRuntime: timeout\n'; exec sleep 30;;
 esac
 `)
	a.devices["one"] = DeviceInfo{Serial: "one", Model: "Phone one", State: "device"}
	a.devices["two"] = DeviceInfo{Serial: "two", Model: "Phone two", State: "device"}
	t.Cleanup(func() { a.requestQuit(); a.stopAllADBLogs() })
	if err := a.startADBLogs("one"); err != nil {
		t.Fatal(err)
	}
	a.adbLogs.mu.Lock()
	first := a.adbLogs.sessions["one"]
	a.adbLogs.mu.Unlock()
	if err := a.startADBLogs("one"); err != nil {
		t.Fatal(err)
	}
	a.adbLogs.mu.Lock()
	same := first == a.adbLogs.sessions["one"]
	a.adbLogs.mu.Unlock()
	if !same {
		t.Fatal("duplicate logcat session")
	}
	if err := a.startADBLogs("two"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for len(a.adbLogs.snapshot(ADBLogFilter{}).Rows) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond * 5)
	}
	rows := a.adbLogs.snapshot(ADBLogFilter{App: "com.example"}).Rows
	if len(rows) != 2 || rows[0].App != "com.example:worker" {
		t.Fatalf("mapping/capture failed %+v", rows)
	}
	// No Lane exists: collecting logs must not enable or depend on weak networking.
	if len(a.lanes) != 0 {
		t.Fatal("log capture changed weak network")
	}
	a.stopADBLogs("one")
	waitADBLogState(t, a, "one", "stopped")
	select {
	case <-first.done:
	case <-time.After(4 * time.Second):
		t.Fatal("logcat process did not exit after stop")
	}
	if err := a.startADBLogs("one"); err != nil {
		t.Fatal(err)
	}
	waitADBLogState(t, a, "one", "running")
	a.requestQuit()
	waitADBLogState(t, a, "one", "stopped")
	waitADBLogState(t, a, "two", "stopped")
	if err := a.startADBLogs("one"); err == nil {
		t.Fatal("started capture after quit")
	}
}
func TestADBLogErrorsAndAPI(t *testing.T) {
	a := newTestApp(t, "weak")
	a.adb = fakeLogADB(t, "echo 'device offline' >&2\nexit 7\n")
	a.devices["one"] = DeviceInfo{Serial: "one", State: "device"}
	t.Cleanup(func() { a.requestQuit(); a.stopAllADBLogs() })
	if err := a.startADBLogs("missing"); err == nil {
		t.Fatal("accepted unknown device")
	}
	if err := a.startADBLogs("one"); err != nil {
		t.Fatal(err)
	}
	snapshot := waitADBLogState(t, a, "one", "retrying")
	if !strings.Contains(snapshot.Sessions[0].Error, "device offline") {
		t.Fatal("stderr lost")
	}
	a.adbLogs.append(ADBLog{Serial: "one", Level: "E", App: "com.example", Message: "timeout"})
	a.adbLogs.append(ADBLog{Serial: "two", Level: "I", Message: "info"})
	handler := a.routes()
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/api/logs?serial=one&level=E&q=timeout", 200}, {"GET", "/api/logs?level=BAD", 400}, {"POST", "/api/logs/missing/start", 400}, {"POST", "/api/logs/one/stop", 200}, {"POST", "/api/logs/clear?serial=one", 200}} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.method == http.MethodGet && tc.want == 200 && !strings.Contains(w.Body.String(), "timeout") {
			t.Fatal("filtered API missing result")
		}
	}
	if got := a.adbLogs.snapshot(ADBLogFilter{}).Rows; len(got) != 1 || got[0].Serial != "two" {
		t.Fatal("API clear affected other device")
	}
	a.stopADBLogs("one")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.scanADBLogs(cancelled, &ADBLogSession{}, strings.NewReader("10-12 00:01:02.123 123 456 E Test: ignored\n")); err != context.Canceled {
		t.Fatalf("cancel error %v", err)
	}
}

func TestADBLogReconnect(t *testing.T) {
	a := newTestApp(t, "weak")
	marker := filepath.Join(t.TempDir(), "attempt")
	quoted := "'" + strings.ReplaceAll(marker, "'", "'\"'\"'") + "'"
	a.adb = fakeLogADB(t, `case "$*" in
 *"shell ps"*) printf 'PID NAME\n123 com.example\n'; exit 0;;
 esac
 if [ ! -f `+quoted+` ]; then touch `+quoted+`; echo 'device offline' >&2; exit 1; fi
 printf '10-12 01:00:00.000 123 123 E Test: reconnected\n'
 exec sleep 30
 `)
	a.devices["one"] = DeviceInfo{Serial: "one", Model: "Phone", State: "device"}
	t.Cleanup(func() { a.requestQuit(); a.stopAllADBLogs() })
	a.adbLogs.append(ADBLog{Serial: "one", Level: "I", Message: "before disconnect"})
	if err := a.startADBLogs("one"); err != nil {
		t.Fatal(err)
	}
	waitADBLogState(t, a, "one", "retrying")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		rows := a.adbLogs.snapshot(ADBLogFilter{}).Rows
		if len(rows) == 2 && rows[0].Message == "reconnected" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("failed to reconnect or lost previous logs")
}
