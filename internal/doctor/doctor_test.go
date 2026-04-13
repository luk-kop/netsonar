package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDoctorConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func baseEnv() Env {
	files := map[string]string{
		"/proc/sys/net/ipv4/ping_group_range": "0 2147483647\n",
		"/proc/self/status":                   "Name:\tnetsonar\nCapEff:\t0000000000002000\n",
		"/etc/resolv.conf":                    "nameserver 10.0.0.10\n",
	}
	return Env{
		ReadFile: func(path string) ([]byte, error) {
			if data, ok := files[path]; ok {
				return []byte(data), nil
			}
			return nil, os.ErrNotExist
		},
		Getuid:                func() int { return 10001 },
		Getgid:                func() int { return 10001 },
		Getgroups:             func() ([]int, error) { return []int{10001, 20000}, nil },
		OpenUnprivilegedICMP:  func() error { return nil },
		CheckRawICMPPMTUProbe: func() error { return nil },
		ListenTCP:             func(string) error { return nil },
	}
}

func TestRunWithEnv_ConfigLoadFailureIsDoctorFailure(t *testing.T) {
	result := RunWithEnv(filepath.Join(t.TempDir(), "missing.yaml"), baseEnv())

	if result.OK() {
		t.Fatal("expected result to fail when config cannot be loaded")
	}
	if got := findCheck(result, "Config", "load config"); got == nil || got.Severity != Fail {
		t.Fatalf("config load check = %+v, want FAIL", got)
	}
}

func TestRunWithEnv_NoMTUSkipsRawICMPFailure(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: tcp-only
    address: example.com:443
    probe_type: tcp
`)
	env := baseEnv()
	env.CheckRawICMPPMTUProbe = func() error { return errors.New("operation not permitted") }

	result := RunWithEnv(path, env)

	if !result.OK() {
		t.Fatalf("expected no-MTU config to pass, checks: %+v", result.Checks)
	}
	if got := findCheck(result, "MTU", "environment"); got == nil || got.Severity != Skip {
		t.Fatalf("MTU environment check = %+v, want SKIP", got)
	}
}

func TestRunWithEnv_MTURequiresRawICMPPMTUProbe(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 10s
targets:
  - name: mtu-target
    address: 127.0.0.1
    probe_type: mtu
    probe_opts:
      icmp_payload_sizes: [1472, 1392]
      expected_min_mtu: 1400
`)
	env := baseEnv()
	env.CheckRawICMPPMTUProbe = func() error { return errors.New("operation not permitted") }

	result := RunWithEnv(path, env)

	if result.OK() {
		t.Fatal("expected MTU config to fail when raw ICMP PMTU check fails")
	}
	if got := findCheck(result, "MTU", "raw ICMP + PMTUDISC"); got == nil || got.Severity != Fail {
		t.Fatalf("raw ICMP check = %+v, want FAIL", got)
	}
}

func TestRunWithEnv_ICMPChecksSupplementaryGroups(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: icmp-target
    address: 127.0.0.1
    probe_type: icmp
`)
	env := baseEnv()
	env.Getgid = func() int { return 10001 }
	env.Getgroups = func() ([]int, error) { return []int{30000}, nil }
	env.ReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/net/ipv4/ping_group_range":
			return []byte("30000 30000\n"), nil
		case "/proc/self/status":
			return []byte("CapEff:\t0000000000000000\n"), nil
		case "/etc/resolv.conf":
			return []byte("nameserver 10.0.0.10\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	result := RunWithEnv(path, env)

	if !result.OK() {
		t.Fatalf("expected supplementary group in ping_group_range to pass, checks: %+v", result.Checks)
	}
	if got := findCheck(result, "ICMP", "ping_group_range"); got == nil || got.Severity != Pass {
		t.Fatalf("ping_group_range check = %+v, want PASS", got)
	}
}

func TestRunWithEnv_DNSWithoutSystemResolverFailsOnlyWhenNeeded(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: dns-target
    address: example.com
    probe_type: dns
    probe_opts:
      dns_query_type: A
`)
	env := baseEnv()
	env.ReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/etc/resolv.conf":
			return []byte("# empty\n"), nil
		case "/proc/sys/net/ipv4/ping_group_range":
			return []byte("0 2147483647\n"), nil
		case "/proc/self/status":
			return []byte("CapEff:\t0000000000000000\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	result := RunWithEnv(path, env)

	if result.OK() {
		t.Fatal("expected DNS target without dns_server to fail when resolv.conf has no nameservers")
	}
	if got := findCheck(result, "DNS", "resolv.conf nameservers"); got == nil || got.Severity != Fail {
		t.Fatalf("DNS nameserver check = %+v, want FAIL", got)
	}
}

func TestRunWithEnv_DNSWithCustomServerWarnsOnMissingSystemResolver(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: dns-target
    address: example.com
    probe_type: dns
    probe_opts:
      dns_query_type: A
      dns_server: "10.0.0.10:53"
`)
	env := baseEnv()
	env.ReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/etc/resolv.conf":
			return []byte("# empty\n"), nil
		case "/proc/sys/net/ipv4/ping_group_range":
			return []byte("0 2147483647\n"), nil
		case "/proc/self/status":
			return []byte("CapEff:\t0000000000000000\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	result := RunWithEnv(path, env)

	if !result.OK() {
		t.Fatalf("expected DNS target with custom dns_server to warn, not fail; checks: %+v", result.Checks)
	}
	if got := findCheck(result, "DNS", "resolv.conf nameservers"); got == nil || got.Severity != Warn {
		t.Fatalf("DNS nameserver check = %+v, want WARN", got)
	}
}

func TestRunWithEnv_PingGroupRangeDisabledFailsForICMPTarget(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: icmp-target
    address: 127.0.0.1
    probe_type: icmp
`)
	env := baseEnv()
	env.ReadFile = func(path string) ([]byte, error) {
		switch path {
		case "/proc/sys/net/ipv4/ping_group_range":
			return []byte("1 0\n"), nil
		case "/proc/self/status":
			return []byte("CapEff:\t0000000000000000\n"), nil
		case "/etc/resolv.conf":
			return []byte("nameserver 10.0.0.10\n"), nil
		default:
			return nil, os.ErrNotExist
		}
	}

	result := RunWithEnv(path, env)

	if result.OK() {
		t.Fatal("expected disabled ping_group_range to fail for ICMP target")
	}
	if got := findCheck(result, "ICMP", "ping_group_range"); got == nil || got.Severity != Fail {
		t.Fatalf("ping_group_range check = %+v, want FAIL", got)
	}
}

func TestRunWithEnv_PlatformUnsupportedAddsSingleWarningPerSection(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  default_interval: 30s
  default_timeout: 10s
targets:
  - name: icmp-target
    address: 127.0.0.1
    probe_type: icmp
  - name: mtu-target
    address: 127.0.0.1
    probe_type: mtu
    probe_opts:
      icmp_payload_sizes: [1472, 1392]
      expected_min_mtu: 1400
`)
	env := baseEnv()
	env.OpenUnprivilegedICMP = nil
	env.CheckRawICMPPMTUProbe = nil
	env.ReadFile = func(path string) ([]byte, error) {
		return nil, os.ErrNotExist
	}

	result := RunWithEnv(path, env)

	if !result.OK() {
		t.Fatalf("unsupported platform checks should warn, not fail; checks: %+v", result.Checks)
	}
	if got := countSectionChecks(result, "ICMP"); got != 1 {
		t.Fatalf("ICMP checks = %d, want 1; checks: %+v", got, result.Checks)
	}
	if got := countSectionChecks(result, "MTU"); got != 1 {
		t.Fatalf("MTU checks = %d, want 1; checks: %+v", got, result.Checks)
	}
	if got := findCheck(result, "ICMP", "environment"); got == nil || got.Severity != Warn {
		t.Fatalf("ICMP unsupported check = %+v, want WARN", got)
	}
	if got := findCheck(result, "MTU", "raw ICMP + PMTUDISC"); got == nil || got.Severity != Warn {
		t.Fatalf("MTU unsupported check = %+v, want WARN", got)
	}
}

func TestRunWithEnv_ListenAddrBindFailureFails(t *testing.T) {
	path := writeDoctorConfig(t, `
agent:
  listen_addr: ":12345"
  default_interval: 30s
  default_timeout: 5s
targets:
  - name: tcp-only
    address: example.com:443
    probe_type: tcp
`)
	env := baseEnv()
	env.ListenTCP = func(addr string) error {
		if addr != ":12345" {
			t.Fatalf("ListenTCP addr = %q, want :12345", addr)
		}
		return errors.New("bind: address already in use")
	}

	result := RunWithEnv(path, env)

	if result.OK() {
		t.Fatal("expected listen bind failure to fail doctor")
	}
	if got := findCheck(result, "ListenAddr", "bind :12345"); got == nil || got.Severity != Fail {
		t.Fatalf("listen check = %+v, want FAIL", got)
	}
}

func TestResultWriteText(t *testing.T) {
	result := Result{
		ConfigPath: "/tmp/config.yaml",
		Checks: []Check{
			{Section: "Config", Name: "load config", Severity: Pass, Detail: "targets=0"},
			{Section: "MTU", Name: "environment", Severity: Skip, Detail: "no mtu targets in config"},
		},
	}

	var b strings.Builder
	result.WriteText(&b)
	output := b.String()

	for _, want := range []string{
		"NetSonar doctor",
		"Config path: /tmp/config.yaml",
		"[PASS] load config: targets=0",
		"[SKIP] environment: no mtu targets in config",
		"Result: OK",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want substring %q", output, want)
		}
	}
}

func findCheck(result Result, section, name string) *Check {
	for i := range result.Checks {
		if result.Checks[i].Section == section && result.Checks[i].Name == name {
			return &result.Checks[i]
		}
	}
	return nil
}

func countSectionChecks(result Result, section string) int {
	count := 0
	for _, check := range result.Checks {
		if check.Section == section {
			count++
		}
	}
	return count
}
