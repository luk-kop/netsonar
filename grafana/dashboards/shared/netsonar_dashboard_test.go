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
	ID              int                       `json:"id"`
	Title           string                    `json:"title"`
	Type            string                    `json:"type"`
	Targets         []dashboardTarget         `json:"targets"`
	FieldConfig     fieldConfig               `json:"fieldConfig"`
	GridPos         gridPos                   `json:"gridPos"`
	Options         panelOptions              `json:"options"`
	Transformations []dashboardTransformation `json:"transformations"`
}

type dashboardTarget struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
	RefID        string `json:"refId"`
	Instant      bool   `json:"instant"`
	Format       string `json:"format"`
}

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type panelOptions struct {
	Legend    legendOptions  `json:"legend"`
	Tooltip   tooltipOptions `json:"tooltip"`
	ColorMode string         `json:"colorMode"`
	TextMode  string         `json:"textMode"`
}

type legendOptions struct {
	DisplayMode string   `json:"displayMode"`
	Placement   string   `json:"placement"`
	Calcs       []string `json:"calcs"`
}

type tooltipOptions struct {
	Mode string `json:"mode"`
}

type dashboardTransformation struct {
	ID      string               `json:"id"`
	Options transformationOption `json:"options"`
}

type transformationOption struct {
	ExcludeByName map[string]bool   `json:"excludeByName"`
	IndexByName   map[string]int    `json:"indexByName"`
	RenameByName  map[string]string `json:"renameByName"`
}

type fieldConfig struct {
	Defaults  fieldDefaults   `json:"defaults"`
	Overrides []fieldOverride `json:"overrides"`
}

type fieldDefaults struct {
	Custom     map[string]any  `json:"custom"`
	Color      map[string]any  `json:"color"`
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
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http", network_path="direct"}[5m]) unless (probe_timed_out{job=~"$job", probe_type="http", network_path="direct"} == 1)`,
		},
		78: {
			title:  "HTTP Response Body Duration median (5m)",
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http_body"}[5m]) unless (probe_timed_out{job=~"$job", probe_type="http_body"} == 1)`,
		},
		83: {
			title:  "HTTP Duration median (5m) (Proxy)",
			median: `quantile_over_time(0.5, probe_duration_seconds{job=~"$job", probe_type="http", network_path="proxy"}[5m]) unless (probe_timed_out{job=~"$job", probe_type="http", network_path="proxy"} == 1)`,
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

func TestNetsonarDashboardCriticalAndTimedOutStatsAreSeparatePanels(t *testing.T) {
	dash := loadDashboard(t)

	critical := findPanel(t, dash, 5)
	if critical.Title != "Critical Failures" || len(critical.Targets) != 1 {
		t.Fatalf("panel 5 = %q with %d targets, want Critical Failures with one target", critical.Title, len(critical.Targets))
	}
	assertTarget(t, critical.Targets[0], "A", `count(probe_success{job=~"$job", impact="critical"} == 0) OR on() vector(0)`, "Critical Failures")

	timedOut := findPanel(t, dash, 218)
	if timedOut.Title != "Timed Out" || len(timedOut.Targets) != 1 {
		t.Fatalf("panel 218 = %q with %d targets, want Timed Out with one target", timedOut.Title, len(timedOut.Targets))
	}
	assertTarget(t, timedOut.Targets[0], "B", `count(probe_timed_out{job=~"$job"} == 1) OR on() vector(0)`, "Timed Out")

	expectedGrid := map[int]gridPos{
		1:   {H: 4, W: 6, X: 0, Y: 1},
		2:   {H: 4, W: 6, X: 6, Y: 1},
		3:   {H: 4, W: 6, X: 12, Y: 1},
		4:   {H: 4, W: 6, X: 18, Y: 1},
		5:   {H: 4, W: 5, X: 0, Y: 5},
		218: {H: 4, W: 5, X: 5, Y: 5},
		8:   {H: 4, W: 5, X: 10, Y: 5},
		9:   {H: 4, W: 4, X: 15, Y: 5},
		6:   {H: 4, W: 5, X: 19, Y: 5},
	}
	for id, want := range expectedGrid {
		panel := findPanel(t, dash, id)
		if panel.GridPos != want {
			t.Fatalf("panel %d grid = %+v, want %+v", id, panel.GridPos, want)
		}
	}

	for id, title := range map[int]string{8: "Config", 9: "Config Age"} {
		panel := findPanel(t, dash, id)
		if panel.Title != title || panel.Options.TextMode != "value" {
			t.Fatalf("panel %d title/textMode = %q/%q, want %q/value", id, panel.Title, panel.Options.TextMode, title)
		}
	}
}

func TestNetsonarStatusTableDoesNotExposePrimaryLatencyColumns(t *testing.T) {
	dash := loadDashboard(t)
	panel := findPanel(t, dash, 7)

	for _, target := range panel.Targets {
		if target.RefID == "B" {
			t.Fatalf("status table still has Primary Latency target B: %q", target.Expr)
		}
	}
	for _, forbidden := range []string{"Primary Latency", "Latency Signal"} {
		if hasOverride(panel, forbidden) {
			t.Fatalf("status table still has override for %q", forbidden)
		}
	}
}

func TestNetsonarStatusTableHidesDuplicateJoinedTagColumns(t *testing.T) {
	dash := loadDashboard(t)
	panel := findPanel(t, dash, 7)
	organize := firstOrganize(t, panel)
	excluded := organize.Options.ExcludeByName

	for _, name := range []string{"target_type 1", "target_type 2", "target_type 3", "target_type 4", "target_type 5", "target_type 6"} {
		if !excluded[name] {
			t.Fatalf("status table does not exclude duplicate joined tag column %q", name)
		}
	}
	if excluded["target_type"] {
		t.Fatal("status table excludes base target_type column, want exactly one visible Target Type column")
	}
	if got := organize.Options.IndexByName["target_type"]; got != 17 {
		t.Fatalf("target_type index = %d, want 17", got)
	}
	if got := organize.Options.RenameByName["target_type"]; got != "Target Type" {
		t.Fatalf("target_type rename = %q, want Target Type", got)
	}
	for field, want := range map[string]int{
		"target":         3,
		"target 1":       3,
		"network_path":   4,
		"network_path 1": 4,
	} {
		if got := organize.Options.IndexByName[field]; got != want {
			t.Fatalf("status table index %q = %d, want %d", field, got, want)
		}
	}
}

func TestNetsonarDashboardProxiedColumnsMapProxyPathToYes(t *testing.T) {
	dash := loadDashboard(t)

	expected := map[int][]string{
		7:   {"network_path", "network_path 1"},
		217: {"network_path 1"},
		60:  {"network_path"},
		61:  {"network_path"},
	}

	for panelID, fields := range expected {
		panel := findPanel(t, dash, panelID)
		renames := firstOrganize(t, panel).Options.RenameByName
		for _, field := range fields {
			if got := renames[field]; got != "Proxied" {
				t.Fatalf("panel %d rename %q = %q, want Proxied", panelID, field, got)
			}
		}
		assertProxiedOverride(t, panel)
	}
}

func TestNetsonarHTTPDetailsProbeInfoAndFixedDurationBackgrounds(t *testing.T) {
	dash := loadDashboardFile(t, "netsonar-http-details.json")

	info := findPanel(t, dash, 8)
	if info.Title != "Probe Info - $target_name" || info.Type != "table" {
		t.Fatalf("panel 8 = %q/%q, want Probe Info - $target_name/table", info.Title, info.Type)
	}
	if info.GridPos.Y != 0 || info.GridPos.X != 0 || info.GridPos.W != 24 {
		t.Fatalf("Probe Info grid = %+v, want full-width top panel", info.GridPos)
	}
	if len(info.Targets) != 1 {
		t.Fatalf("Probe Info targets = %d, want 1", len(info.Targets))
	}
	if !info.Targets[0].Instant || info.Targets[0].Format != "table" {
		t.Fatalf("Probe Info target instant/format = %v/%q, want true/table", info.Targets[0].Instant, info.Targets[0].Format)
	}
	renames := firstOrganize(t, info).Options.RenameByName
	if got := renames["network_path"]; got != "Path" {
		t.Fatalf("Probe Info network_path rename = %q, want Path", got)
	}
	if excluded := firstOrganize(t, info).Options.ExcludeByName["probe_type"]; !excluded {
		t.Fatal("Probe Info exposes probe_type column, want it hidden")
	}
	if excluded := firstOrganize(t, info).Options.ExcludeByName["target_name"]; !excluded {
		t.Fatal("Probe Info exposes target_name column, want it hidden")
	}
	expectedInfoIndex := map[string]int{
		"Value":          0,
		"target":         1,
		"network_path":   2,
		"impact":         3,
		"service":        4,
		"scope":          5,
		"target_type":    6,
		"target_region":  7,
		"target_account": 8,
	}
	for field, want := range expectedInfoIndex {
		if got := firstOrganize(t, info).Options.IndexByName[field]; got != want {
			t.Fatalf("Probe Info index %q = %d, want %d", field, got, want)
		}
	}
	if hasOverride(info, "Proxied") {
		t.Fatal("Probe Info still has Proxied override")
	}

	for _, id := range []int{1, 2, 3, 4} {
		panel := findPanel(t, dash, id)
		if panel.Type != "stat" || panel.Options.ColorMode != "background" {
			t.Fatalf("panel %d type/colorMode = %q/%q, want stat/background", id, panel.Type, panel.Options.ColorMode)
		}
		color := panel.FieldConfig.Defaults.Color
		if color["mode"] != "fixed" || color["fixedColor"] != "blue" {
			t.Fatalf("panel %d color = %+v, want fixed blue", id, color)
		}
		steps := panel.FieldConfig.Defaults.Thresholds.Steps
		if len(steps) != 1 || steps[0].Color != "blue" {
			t.Fatalf("panel %d thresholds = %+v, want single blue step", id, steps)
		}
		if panel.GridPos.Y != 4 {
			t.Fatalf("panel %d y = %d, want 4 below Probe Info", id, panel.GridPos.Y)
		}
	}

	summary := findPanel(t, dash, 9)
	if summary.Title != "Phase Summary - $target_name" || summary.Type != "table" {
		t.Fatalf("panel 9 = %q/%q, want Phase Summary - $target_name/table", summary.Title, summary.Type)
	}
	if summary.GridPos != (gridPos{H: 8, W: 24, X: 0, Y: 28}) {
		t.Fatalf("Phase Summary grid = %+v, want full-width bottom panel at y=28", summary.GridPos)
	}
	organize := lastOrganize(t, summary)
	for field, want := range map[string]int{"phase": 0, "Value #A": 1, "Value #B": 2, "Value #C": 3} {
		if got := organize.Options.IndexByName[field]; got != want {
			t.Fatalf("Phase Summary index %q = %d, want %d", field, got, want)
		}
	}
	for field, want := range map[string]string{"phase": "Phase", "Value #A": "Last", "Value #B": "Mean", "Value #C": "Max"} {
		if got := organize.Options.RenameByName[field]; got != want {
			t.Fatalf("Phase Summary rename %q = %q, want %q", field, got, want)
		}
	}
	if len(summary.Targets) != 3 {
		t.Fatalf("Phase Summary targets = %d, want 3", len(summary.Targets))
	}
	assertTarget(t, summary.Targets[0], "A", `sum by (phase) (probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}) or label_replace(sum by (target_name) (probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}), "phase", "zz_TOTAL", "target_name", ".*")`, "")
	assertTarget(t, summary.Targets[1], "B", `sum by (phase) (avg_over_time(probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}[$__range])) or label_replace(sum by (target_name) (avg_over_time(probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}[$__range])), "phase", "zz_TOTAL", "target_name", ".*")`, "")
	assertTarget(t, summary.Targets[2], "C", `sum by (phase) (max_over_time(probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}[$__range])) or label_replace(sum by (target_name) (max_over_time(probe_phase_duration_seconds{job=~"$job", probe_type=~"http|http_body", target_name="$target_name"}[$__range])), "phase", "zz_TOTAL", "target_name", ".*")`, "")
}

func TestNetsonarDashboardTLSCertificatePhasePanels(t *testing.T) {
	dash := loadDashboard(t)

	breakdown := findPanel(t, dash, 207)
	if breakdown.Title != "TLS Cert Phase Breakdown" || breakdown.Type != "table" {
		t.Fatalf("panel 207 = %q/%q, want TLS Cert Phase Breakdown/table", breakdown.Title, breakdown.Type)
	}
	assertTarget(t, breakdown.Targets[0], "A", `label_join(label_replace(probe_success{job=~"$job", probe_type="tls_cert"}, "phase", "Status", "__name__", ".*"), "target_path", " / ", "target_name", "network_path") or label_join(probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}, "target_path", " / ", "target_name", "network_path") or label_replace(label_join(sum by (target_name, network_path) (probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}), "target_path", " / ", "target_name", "network_path"), "phase", "total_phases", "__name__", ".*")`, "")

	timing := findPanel(t, dash, 208)
	if timing.Title != "TLS Phase Timing" || timing.Type != "timeseries" {
		t.Fatalf("panel 208 = %q/%q, want TLS Phase Timing/timeseries", timing.Title, timing.Type)
	}
	assertTarget(t, timing.Targets[0], "A", `probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}`, "{{target_name}} - {{network_path}} - {{phase}}")
}

func TestNetsonarDashboardProbeSectionsHavePhasePanels(t *testing.T) {
	dash := loadDashboard(t)

	expected := map[int]struct {
		title  string
		kind   string
		expr   string
		legend string
	}{
		201: {
			title: "HTTP Phase Breakdown (Direct)",
			kind:  "table",
			expr:  `label_replace(probe_success{job=~"$job", probe_type="http", network_path="direct"}, "phase", "Status", "__name__", ".*") or probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="direct"} or label_replace(sum by (target_name) (probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="direct"}), "phase", "total_phases", "__name__", ".*")`,
		},
		206: {
			title:  "HTTP Phase Timing (Direct)",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="direct"}`,
			legend: "{{target_name}} - {{phase}}",
		},
		204: {
			title: "HTTP Phase Breakdown (Proxy)",
			kind:  "table",
			expr:  `label_replace(probe_success{job=~"$job", probe_type="http", network_path="proxy"}, "phase", "Status", "__name__", ".*") or probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="proxy"} or label_replace(sum by (target_name) (probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="proxy"}), "phase", "total_phases", "__name__", ".*")`,
		},
		205: {
			title:  "HTTP Phase Timing (Proxy)",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="http", network_path="proxy"}`,
			legend: "{{target_name}} - {{phase}}",
		},
		209: {
			title: "HTTP Response Body Phase Breakdown",
			kind:  "table",
			expr:  `label_join(label_replace(probe_success{job=~"$job", probe_type="http_body"}, "phase", "Status", "__name__", ".*"), "target_path", " / ", "target_name", "network_path") or label_join(probe_phase_duration_seconds{job=~"$job", probe_type="http_body"}, "target_path", " / ", "target_name", "network_path") or label_replace(label_join(sum by (target_name, network_path) (probe_phase_duration_seconds{job=~"$job", probe_type="http_body"}), "target_path", " / ", "target_name", "network_path"), "phase", "total_phases", "__name__", ".*")`,
		},
		210: {
			title:  "HTTP Response Body Phase Timing",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="http_body"}`,
			legend: "{{target_name}} - {{network_path}} - {{phase}}",
		},
		211: {
			title: "Proxy CONNECT Phase Breakdown",
			kind:  "table",
			expr:  `label_replace(probe_success{job=~"$job", probe_type="proxy_connect"}, "phase", "Status", "__name__", ".*") or probe_phase_duration_seconds{job=~"$job", probe_type="proxy_connect"} or label_replace(sum by (target_name) (probe_phase_duration_seconds{job=~"$job", probe_type="proxy_connect"}), "phase", "total_phases", "__name__", ".*")`,
		},
		77: {
			title:  "Proxy CONNECT Phase Timing",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="proxy_connect"}`,
			legend: "{{target_name}} — {{phase}}",
		},
		212: {
			title: "TCP Phase Breakdown",
			kind:  "table",
			expr:  `label_replace(probe_success{job=~"$job", probe_type="tcp"}, "phase", "Status", "__name__", ".*") or probe_phase_duration_seconds{job=~"$job", probe_type="tcp"} or label_replace(sum by (target_name) (probe_phase_duration_seconds{job=~"$job", probe_type="tcp"}), "phase", "total_phases", "__name__", ".*")`,
		},
		213: {
			title:  "TCP Phase Timing",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="tcp"}`,
			legend: "{{target_name}} - {{phase}}",
		},
		207: {
			title: "TLS Cert Phase Breakdown",
			kind:  "table",
			expr:  `label_join(label_replace(probe_success{job=~"$job", probe_type="tls_cert"}, "phase", "Status", "__name__", ".*"), "target_path", " / ", "target_name", "network_path") or label_join(probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}, "target_path", " / ", "target_name", "network_path") or label_replace(label_join(sum by (target_name, network_path) (probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}), "target_path", " / ", "target_name", "network_path"), "phase", "total_phases", "__name__", ".*")`,
		},
		208: {
			title:  "TLS Phase Timing",
			kind:   "timeseries",
			expr:   `probe_phase_duration_seconds{job=~"$job", probe_type="tls_cert"}`,
			legend: "{{target_name}} - {{network_path}} - {{phase}}",
		},
	}

	for id, want := range expected {
		panel := findPanel(t, dash, id)
		if panel.Title != want.title || panel.Type != want.kind {
			t.Fatalf("panel %d = %q/%q, want %q/%q", id, panel.Title, panel.Type, want.title, want.kind)
		}
		assertTarget(t, panel.Targets[0], "A", want.expr, want.legend)
	}
}

func TestNetsonarDashboardPhaseTimingPanelsUseTableLegend(t *testing.T) {
	dash := loadDashboard(t)

	wantCalcs := []string{"mean", "max", "lastNotNull"}
	for _, id := range []int{206, 205, 210, 77, 213, 208} {
		panel := findPanel(t, dash, id)
		if panel.Options.Legend.DisplayMode != "table" {
			t.Fatalf("panel %d %q legend displayMode = %q, want table", id, panel.Title, panel.Options.Legend.DisplayMode)
		}
		if panel.Options.Legend.Placement != "bottom" {
			t.Fatalf("panel %d %q legend placement = %q, want bottom", id, panel.Title, panel.Options.Legend.Placement)
		}
		if len(panel.Options.Legend.Calcs) != len(wantCalcs) {
			t.Fatalf("panel %d %q legend calcs = %#v, want %#v", id, panel.Title, panel.Options.Legend.Calcs, wantCalcs)
		}
		for i, want := range wantCalcs {
			if panel.Options.Legend.Calcs[i] != want {
				t.Fatalf("panel %d %q legend calcs = %#v, want %#v", id, panel.Title, panel.Options.Legend.Calcs, wantCalcs)
			}
		}
		if panel.Options.Tooltip.Mode != "multi" {
			t.Fatalf("panel %d %q tooltip mode = %q, want multi", id, panel.Title, panel.Options.Tooltip.Mode)
		}
	}
}

func TestNetsonarDashboardPhaseBreakdownTablesShowStatusAfterTarget(t *testing.T) {
	dash := loadDashboard(t)

	for _, id := range []int{201, 204, 209, 211, 212, 207} {
		panel := findPanel(t, dash, id)
		index := lastOrganizeIndex(t, panel)

		if got := index["Status"]; got != 1 {
			t.Fatalf("panel %d %q Status column index = %d, want 1", id, panel.Title, got)
		}
		if !hasOverride(panel, "Status") {
			t.Fatalf("panel %d %q has no Status field override", id, panel.Title)
		}
	}
}

func TestNetsonarDashboardTLSCertificateExpirySortColumnFirst(t *testing.T) {
	dash := loadDashboard(t)

	for _, id := range []int{60, 61} {
		panel := findPanel(t, dash, id)
		index := firstOrganizeIndex(t, panel)
		if got := index["Value"]; got != 0 {
			t.Fatalf("panel %d Value column index = %d, want 0", id, got)
		}
	}
}

func TestNetsonarDashboardTCPPanelsUseCurrentScopeValues(t *testing.T) {
	dash := loadDashboard(t)

	expected := map[int]struct {
		title string
		expr  string
	}{
		10: {
			title: "TCP Duration — Local",
			expr:  `probe_duration_seconds{job=~"$job", probe_type="tcp", scope="local"} unless (probe_timed_out{job=~"$job", probe_type="tcp", scope="local"} == 1)`,
		},
		11: {
			title: "TCP Duration — External",
			expr:  `probe_duration_seconds{job=~"$job", probe_type="tcp", scope="external"} unless (probe_timed_out{job=~"$job", probe_type="tcp", scope="external"} == 1)`,
		},
		12: {
			title: "TCP Duration — All Targets",
			expr:  `probe_duration_seconds{job=~"$job", probe_type="tcp"} unless (probe_timed_out{job=~"$job", probe_type="tcp"} == 1)`,
		},
	}

	for id, want := range expected {
		panel := findPanel(t, dash, id)
		if panel.Title != want.title {
			t.Fatalf("panel %d title = %q, want %q", id, panel.Title, want.title)
		}
		assertTarget(t, panel.Targets[0], "A", want.expr, "{{target_name}}")
	}
}

func TestNetsonarDashboardMTUStatusColumnFirst(t *testing.T) {
	dash := loadDashboard(t)
	panel := findPanel(t, dash, 43)
	index := firstOrganizeIndex(t, panel)

	if got := index["Value #B"]; got != 0 {
		t.Fatalf("panel 43 Status column index = %d, want 0", got)
	}
}

func loadDashboard(t *testing.T) dashboardJSON {
	t.Helper()

	return loadDashboardFile(t, "netsonar.json")
}

func loadDashboardFile(t *testing.T, name string) dashboardJSON {
	t.Helper()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	var dash dashboardJSON
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("parse dashboard JSON: %v", err)
	}
	return dash
}

func firstOrganizeIndex(t *testing.T, panel dashboardPanel) map[string]int {
	t.Helper()

	return firstOrganize(t, panel).Options.IndexByName
}

func firstOrganize(t *testing.T, panel dashboardPanel) dashboardTransformation {
	t.Helper()

	for _, transformation := range panel.Transformations {
		if transformation.ID == "organize" {
			return transformation
		}
	}
	t.Fatalf("panel %d has no organize transformation", panel.ID)
	return dashboardTransformation{}
}

func lastOrganizeIndex(t *testing.T, panel dashboardPanel) map[string]int {
	t.Helper()

	return lastOrganize(t, panel).Options.IndexByName
}

func lastOrganize(t *testing.T, panel dashboardPanel) dashboardTransformation {
	t.Helper()

	for i := len(panel.Transformations) - 1; i >= 0; i-- {
		if panel.Transformations[i].ID == "organize" {
			return panel.Transformations[i]
		}
	}
	t.Fatalf("panel %d has no organize transformation", panel.ID)
	return dashboardTransformation{}
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

func hasOverride(panel dashboardPanel, name string) bool {
	for _, override := range panel.FieldConfig.Overrides {
		if override.Matcher.ID == "byName" && override.Matcher.Options == name {
			return true
		}
	}
	return false
}

func assertProxiedOverride(t *testing.T, panel dashboardPanel) {
	t.Helper()

	override := findOverride(t, panel, "Proxied")
	mappings := overridePropertyValue(t, override, "mappings").([]any)
	options := mappings[0].(map[string]any)["options"].(map[string]any)
	proxy := options["proxy"].(map[string]any)
	direct := options["direct"].(map[string]any)
	if proxy["text"] != "YES" || proxy["color"] != "blue" {
		t.Fatalf("panel %d proxied proxy mapping = %+v, want YES/blue", panel.ID, proxy)
	}
	if direct["text"] != "" || direct["color"] != "transparent" {
		t.Fatalf("panel %d proxied direct mapping = %+v, want blank/transparent", panel.ID, direct)
	}

	cellOptions := overridePropertyValue(t, override, "custom.cellOptions").(map[string]any)
	if cellOptions["type"] != "color-background" {
		t.Fatalf("panel %d proxied cell option = %+v, want color-background", panel.ID, cellOptions)
	}
}

func findOverride(t *testing.T, panel dashboardPanel, name string) fieldOverride {
	t.Helper()

	for _, override := range panel.FieldConfig.Overrides {
		if override.Matcher.ID == "byName" && override.Matcher.Options == name {
			return override
		}
	}
	t.Fatalf("panel %d has no override for %q", panel.ID, name)
	return fieldOverride{}
}

func overridePropertyValue(t *testing.T, override fieldOverride, id string) any {
	t.Helper()

	for _, property := range override.Properties {
		if property.ID == id {
			return property.Value
		}
	}
	t.Fatalf("override %q has no property %q", override.Matcher.Options, id)
	return nil
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
	if got := nestedString(t, custom["thresholdsStyle"], "mode"); got != "off" {
		t.Fatalf("panel %d thresholdsStyle.mode = %q, want off", panel.ID, got)
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
