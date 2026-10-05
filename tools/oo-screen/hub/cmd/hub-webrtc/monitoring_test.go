package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// O1: дашборд і алерти (deploy/monitoring) посилаються лише на метрики, які
// хаб справді віддає. Перейменування в metrics.go без правки моніторингу
// падає тут, а не мовчазним «No data» у Grafana.
func TestMonitoringReferencesExistingMetrics(t *testing.T) {
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(oo_hub_[a-z0-9_]+)"`).FindAllStringSubmatch(string(src), -1) {
		known[m[1]] = true
	}
	ref := regexp.MustCompile(`oo_hub_[a-z0-9_]+`)
	check := func(file, body string) int {
		n := 0
		for _, m := range ref.FindAllString(body, -1) {
			n++
			base := m
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if strings.HasSuffix(m, suf) && known[strings.TrimSuffix(m, suf)] {
					base = strings.TrimSuffix(m, suf)
				}
			}
			if !known[base] {
				t.Errorf("%s: метрики %s хаб не віддає", file, m)
			}
		}
		return n
	}
	alerts, err := os.ReadFile("../../../deploy/monitoring/oo-screen-alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	if check("alerts", string(alerts)) < 10 {
		t.Error("підозріло мало посилань в алертах")
	}
	dash, err := os.ReadFile("../../../deploy/monitoring/oo-screen-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(dash, &d); err != nil {
		t.Fatalf("дашборд не JSON: %v", err)
	}
	n := 0
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			n += check("dashboard", tg.Expr)
		}
	}
	if len(d.Panels) < 10 || n < 15 {
		t.Errorf("дашборд: %d панелей, %d посилань", len(d.Panels), n)
	}
}
