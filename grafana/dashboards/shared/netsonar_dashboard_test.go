package shared

import (
	"encoding/json"
	"os"
	"testing"
)

type dashboardJSON struct {
	Panels []dashboardPanel `json:"panels"`
}

type dashboardPanel struct {
	ID          int               `json:"id"`
	Title       string            `json:"title"`
	Targets     []dashboardTarget `json:"targets"`
	FieldConfig fieldConfig       `json:"fieldConfig"`
}

type dashboardTarget struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
	RefID        string `json:"refId"`
}

type fieldConfig struct {
	Defaults  fieldDefaults   `json:"defaults"`
	Overrides []fieldOverride `json:"overrides"`
}

type fieldDefaults struct {
	Custom     map[string]any  `json:"custom"`
	Thresholds thresholdConfig `json:"thresholds"`
}

type thresholdConfig struct {
	Mode  string          `json:"mode"`
	Steps []thresholdStep `json:"steps"`
}

type thresholdStep struct {
	Color string   `json:"color"`
	Value *float64 `json:"value"`
}

type fieldOverride struct {
	Matcher    overrideMatcher    `json:"matcher"`
	Properties []overrideProperty `json:"properties"`
}

type overrideMatcher struct {
	ID      string `json:"id"`
	Options string `json:"options"`
}

type overrideProperty struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

func TestNetsonarDashboardDurationPanelsUseMedianSeries(t *testing.T) {
	dash := loadDashboard(t)

	expected := map[int]struct {
		title  string
		median string
	}{
		24: {
			title:  "HTTP Duration median (5m) (Direct)",
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http", network_path="direct"}[5m])`,
		},
		78: {
			title:  "HTTP Body Duration median (5m)",
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http_body"}[5m])`,
		},
		83: {
			title:  "Proxy-Path HTTP Duration median (5m)",
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http", network_path="proxy"}[5m])`,
		},
	}

	for id, want := range expected {
		panel := findPanel(t, dash, id)
		if panel.Title != want.title {
			t.Fatalf("panel %d title = %q, want %q", id, panel.Title, want.title)
		}
		if len(panel.Targets) != 1 {
			t.Fatalf("panel %d targets = %d, want 1", id, len(panel.Targets))
		}
		assertTarget(t, panel.Targets[0], "A", want.median, "{{target_name}}")
		assertLatencyPanelStyle(t, panel)
	}
}

func TestNetsonarDashboardSoftMaxOnlyOnSmoothedDurationPanels(t *testing.T) {
	dash := loadDashboard(t)
	allowed := map[int]bool{24: true, 78: true, 83: true}

	for _, panel := range dash.Panels {
		if _, ok := panel.FieldConfig.Defaults.Custom["axisSoftMax"]; ok && !allowed[panel.ID] {
			t.Fatalf("panel %d %q has unexpected axisSoftMax", panel.ID, panel.Title)
		}
	}
}

func loadDashboard(t *testing.T) dashboardJSON {
	t.Helper()

	data, err := os.ReadFile("netsonar.json")
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	var dash dashboardJSON
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("parse dashboard JSON: %v", err)
	}
	return dash
}

func findPanel(t *testing.T, dash dashboardJSON, id int) dashboardPanel {
	t.Helper()

	for _, panel := range dash.Panels {
		if panel.ID == id {
			return panel
		}
	}
	t.Fatalf("panel %d not found", id)
	return dashboardPanel{}
}

func assertTarget(t *testing.T, target dashboardTarget, refID, expr, legend string) {
	t.Helper()

	if target.RefID != refID {
		t.Fatalf("target refID = %q, want %q", target.RefID, refID)
	}
	if target.Expr != expr {
		t.Fatalf("target %s expr = %q, want %q", refID, target.Expr, expr)
	}
	if target.LegendFormat != legend {
		t.Fatalf("target %s legend = %q, want %q", refID, target.LegendFormat, legend)
	}
}

func assertLatencyPanelStyle(t *testing.T, panel dashboardPanel) {
	t.Helper()

	custom := panel.FieldConfig.Defaults.Custom
	if got := number(t, custom["axisSoftMax"]); got != 1.0 {
		t.Fatalf("panel %d axisSoftMax = %v, want 1.0", panel.ID, got)
	}
	if got := nestedString(t, custom["scaleDistribution"], "type"); got != "linear" {
		t.Fatalf("panel %d scaleDistribution.type = %q, want linear", panel.ID, got)
	}
	if got := nestedString(t, custom["thresholdsStyle"], "mode"); got != "line" {
		t.Fatalf("panel %d thresholdsStyle.mode = %q, want line", panel.ID, got)
	}

	thresholds := panel.FieldConfig.Defaults.Thresholds
	if thresholds.Mode != "absolute" || len(thresholds.Steps) != 2 {
		t.Fatalf("panel %d thresholds = %+v, want two absolute steps", panel.ID, thresholds)
	}
	if thresholds.Steps[1].Color != "red" || thresholds.Steps[1].Value == nil || *thresholds.Steps[1].Value != 1.0 {
		t.Fatalf("panel %d threshold step = %+v, want red at 1.0", panel.ID, thresholds.Steps[1])
	}

	if len(panel.FieldConfig.Overrides) != 0 {
		t.Fatalf("panel %d overrides = %d, want 0", panel.ID, len(panel.FieldConfig.Overrides))
	}
}

func nestedString(t *testing.T, value any, key string) string {
	t.Helper()

	m, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value %v is %T, want object", value, value)
	}
	got, ok := m[key].(string)
	if !ok {
		t.Fatalf("value[%q] = %v, want string", key, m[key])
	}
	return got
}

func number(t *testing.T, value any) float64 {
	t.Helper()

	got, ok := value.(float64)
	if !ok {
		t.Fatalf("value %v is %T, want number", value, value)
	}
	return got
}
