package main

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func historyFixture(t *testing.T) (*App, *Lane) {
	t.Helper()
	app := newTestApp(t, "unlimited")
	lane := &Lane{serial: "phone-A", name: "Phone A"}
	statuses := []string{"ok", "failed", "rejected", "active", "failed", "ok"}
	for i, status := range statuses {
		r := app.conns.open(lane, "com.example.app", "api.example.com", "443", i%2 == 1)
		r.Start = int64(i+1) * 1000
		r.down.Store(int64(i+1) * 100)
		r.up.Store(int64(i+1) * 10)
		if status != "active" {
			r.End = r.Start + 100
			r.Status = status
		}
	}
	return app, lane
}

func ids(rows []ConnView) []int64 {
	out := []int64{}
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func TestConnCursorSurvivesNewConnections(t *testing.T) {
	app, lane := historyFixture(t)
	first, sum, next := app.conns.queryPage(ConnFilter{}, 2, 0)
	if !reflect.DeepEqual(ids(first), []int64{6, 5}) || next != 5 || sum.Count != 6 {
		t.Fatalf("first page: %v next=%d summary=%+v", ids(first), next, sum)
	}
	app.conns.open(lane, "com.example.app", "new.example.com", "443", true)
	second, sum, next := app.conns.queryPage(ConnFilter{}, 2, next)
	if !reflect.DeepEqual(ids(second), []int64{4, 3}) || next != 3 || sum.Count != 7 {
		t.Fatalf("second page shifted: %v next=%d summary=%+v", ids(second), next, sum)
	}
	last, _, next := app.conns.queryPage(ConnFilter{}, 2, next)
	if !reflect.DeepEqual(ids(last), []int64{2, 1}) || next != 0 {
		t.Fatalf("last page: %v next=%d", ids(last), next)
	}
	app.conns.clear()
	empty, sum, next := app.conns.queryPage(ConnFilter{}, 2, 3)
	if len(empty) != 0 || sum.Count != 0 || next != 0 {
		t.Fatalf("cleared history: %v %+v %d", empty, sum, next)
	}
}

func TestConnFiltersAndCSVUseSameScope(t *testing.T) {
	app, _ := historyFixture(t)
	filter := ConnFilter{Serial: "phone-A", App: "com.example.app", Host: "API.EXAMPLE", Status: "errors", Shaped: "false", Since: 3000, Until: 5000}
	rows, sum, next := app.conns.queryPage(filter, 1, 0)
	if !reflect.DeepEqual(ids(rows), []int64{5}) || next != 5 || sum.Count != 2 || sum.Failed != 2 || sum.Down != 800 {
		t.Fatalf("filtered page: %v next=%d summary=%+v", ids(rows), next, sum)
	}
	older, sum, next := app.conns.queryPage(filter, 1, next)
	if !reflect.DeepEqual(ids(older), []int64{3}) || next != 0 || sum.Count != 2 {
		t.Fatalf("filtered older page: %v next=%d summary=%+v", ids(older), next, sum)
	}
	var body strings.Builder
	if err := app.conns.writeCSV(&body, filter); err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[1][8] != "300" || records[2][8] != "500" {
		t.Fatalf("CSV scope or order differs: %v", records)
	}
}

func TestHistoryRoutesCursorAndExport(t *testing.T) {
	app, _ := historyFixture(t)
	handler := app.routes()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/api/conns?limit=1&before=6&status=errors&shaped=false&since=3000&until=5000", nil))
	var result struct {
		Rows    []ConnView  `json:"rows"`
		Summary ConnSummary `json:"summary"`
		Next    int64       `json:"next_before"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if page.Code != 200 || !reflect.DeepEqual(ids(result.Rows), []int64{5}) || result.Summary.Count != 2 || result.Next != 5 {
		t.Fatalf("bad response: %d %s", page.Code, page.Body.String())
	}
	export := httptest.NewRecorder()
	handler.ServeHTTP(export, httptest.NewRequest(http.MethodGet, "/api/conns.csv?status=errors&shaped=false&since=3000&until=5000", nil))
	records, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(export.Body.String(), "\uFEFF"))).ReadAll()
	if err != nil || len(records) != 3 {
		t.Fatalf("export: %v %s", err, export.Body.String())
	}
}

func TestInvalidConfigDoesNotPartiallyApply(t *testing.T) {
	app, _ := historyFixture(t)
	before := app.cfg.Get()
	for _, body := range []string{`{"down_kbps":300,"loss_pct":101}`, `{"delay_ms":-1}`, `{"window_kb":0}`, `{"blacklist":["example.com",42]}`, `{"outage":"unknown"}`, `{"blacklist_on":"true"}`} {
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid config %s: %d", body, response.Code)
		}
		if after := app.cfg.Get(); !reflect.DeepEqual(before, after) {
			t.Fatalf("partial config update for %s: %+v", body, after)
		}
	}
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"blacklist":["example.com","example.com"," cdn.example.com "]}`)))
	if response.Code != http.StatusOK || !reflect.DeepEqual(app.cfg.Get().Blacklist, []string{"example.com", "cdn.example.com"}) {
		t.Fatalf("valid list not normalized: %d %s", response.Code, response.Body.String())
	}
}

func TestDeviceRoutesReportUnavailableAndBusy(t *testing.T) {
	app := newTestApp(t, "unlimited")
	app.devices["phone-A"] = DeviceInfo{Serial: "phone-A", State: "device"}
	app.busy["phone-A"] = "existing action"
	for _, url := range []string{"/api/device/missing/on", "/api/device/phone-A/on", "/api/device/phone-A/unknown"} {
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, url, strings.NewReader("{}")))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("operation incorrectly accepted: %s %d", url, response.Code)
		}
	}
	for _, serial := range []string{"missing", "phone-A"} {
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/device/"+serial+"/clear", strings.NewReader(`{"pkg":"com.example.app","launch":true}`)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("clear must reject unavailable or busy device: %s %d", serial, response.Code)
		}
	}
}
