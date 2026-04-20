// Package doctor provides config-aware environment diagnostics.
package doctor

import (
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"netsonar/internal/config"
)

type Severity string

const (
	Pass Severity = "PASS"
	Warn Severity = "WARN"
	Fail Severity = "FAIL"
	Skip Severity = "SKIP"
)

type Check struct {
	Section  string
	Name     string
	Severity Severity
	Detail   string
	Hint     string
}

type Result struct {
	ConfigPath   string
	TargetCounts map[config.ProbeType]int
	Checks       []Check
}

type Options struct {
	ListenAddrOverride string
}

type Env struct {
	ReadFile             func(string) ([]byte, error)
	Getuid               func() int
	Getgid               func() int
	Getgroups            func() ([]int, error)
	OpenUnprivilegedICMP func() error
	CheckMTUPingSocket   func() error
	ListenTCP            func(addr string) error
}

func Run(configPath string) Result {
	return RunWithOptions(configPath, DefaultEnv(), Options{})
}

func RunWithEnv(configPath string, env Env) Result {
	return RunWithOptions(configPath, env, Options{})
}

func RunWithOptions(configPath string, env Env, opts Options) Result {
	env = fillEnvDefaults(env)

	result := Result{
		ConfigPath:   configPath,
		TargetCounts: make(map[config.ProbeType]int),
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		result.add("Config", "load config", Fail, err.Error(), "Fix the configuration file and run doctor again.")
		return result
	}
	if opts.ListenAddrOverride != "" {
		cfg.Agent.ListenAddr = opts.ListenAddrOverride
	}

	for _, target := range cfg.Targets {
		result.TargetCounts[target.ProbeType]++
	}
	result.add("Config", "load config", Pass, fmt.Sprintf("targets=%d", len(cfg.Targets)), "")

	checkProcess(&result, env)
	checkListenAddr(&result, env, cfg.Agent.ListenAddr)
	checkICMP(&result, env)
	checkMTU(&result, env)
	checkDNS(&result, env, cfg.Targets)

	return result
}

func (r Result) OK() bool {
	for _, check := range r.Checks {
		if check.Severity == Fail {
			return false
		}
	}
	return true
}

func (r Result) WriteText(w io.Writer) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

	p("NetSonar doctor\n\n")
	p("Config path: %s\n", r.ConfigPath)
	if len(r.TargetCounts) > 0 {
		p("Targets:\n")
		for _, probeType := range sortedProbeTypes(r.TargetCounts) {
			p("  %s: %d\n", probeType, r.TargetCounts[probeType])
		}
		p("\n")
	}

	sectionOrder := []string{"Config", "Process", "ListenAddr", "ICMP", "MTU", "DNS"}
	for _, section := range sectionOrder {
		checks := r.sectionChecks(section)
		if len(checks) == 0 {
			continue
		}
		p("%s:\n", section)
		for _, check := range checks {
			p("  [%s] %s", check.Severity, check.Name)
			if check.Detail != "" {
				p(": %s", check.Detail)
			}
			p("\n")
			if check.Hint != "" {
				p("    hint: %s\n", check.Hint)
			}
		}
		p("\n")
	}

	if r.OK() {
		p("Result: OK\n")
		return
	}
	p("Result: FAIL\n")
}

func (r *Result) add(section, name string, severity Severity, detail, hint string) {
	r.Checks = append(r.Checks, Check{
		Section:  section,
		Name:     name,
		Severity: severity,
		Detail:   detail,
		Hint:     hint,
	})
}

func (r Result) sectionChecks(section string) []Check {
	var checks []Check
	for _, check := range r.Checks {
		if check.Section == section {
			checks = append(checks, check)
		}
	}
	return checks
}

func fillEnvDefaults(env Env) Env {
	if env.ReadFile == nil {
		env.ReadFile = os.ReadFile
	}
	if env.Getuid == nil {
		env.Getuid = os.Getuid
	}
	if env.Getgid == nil {
		env.Getgid = os.Getgid
	}
	if env.Getgroups == nil {
		env.Getgroups = os.Getgroups
	}
	if env.ListenTCP == nil {
		env.ListenTCP = listenTCP
	}
	return env
}

func checkProcess(result *Result, env Env) {
	uid := env.Getuid()
	gid := env.Getgid()

	groups, err := env.Getgroups()
	if err != nil {
		result.add("Process", "uid/gid", Warn, fmt.Sprintf("uid=%d gid=%d; could not determine supplementary groups: %s", uid, gid, err), "")
		return
	}

	result.add("Process", "uid/gid/groups", Pass, fmt.Sprintf("uid=%d gid=%d groups=%v", uid, gid, uniqueInts(append([]int{gid}, groups...))), "")
}

func checkListenAddr(result *Result, env Env, addr string) {
	if err := env.ListenTCP(addr); err != nil {
		result.add("ListenAddr", "bind "+addr, Fail, err.Error(), "Free the port or change agent.listen_addr.")
		return
	}
	result.add("ListenAddr", "bind "+addr, Pass, "ok", "")
}

func checkICMP(result *Result, env Env) {
	if result.TargetCounts[config.ProbeTypeICMP] == 0 {
		result.add("ICMP", "environment", Skip, "no icmp targets in config", "")
		return
	}
	if env.OpenUnprivilegedICMP == nil {
		result.add("ICMP", "environment", Warn, "not supported on this platform", "ICMP checks require a Linux runtime.")
		return
	}

	checkPingGroupRange(result, env, "ICMP")

	if err := env.OpenUnprivilegedICMP(); err != nil {
		result.add("ICMP", "unprivileged socket", Fail, err.Error(), "Ensure net.ipv4.ping_group_range includes the process effective or supplementary GID.")
		return
	}
	result.add("ICMP", "unprivileged socket", Pass, "ok", "")
}

func checkPingGroupRange(result *Result, env Env, section string) {
	data, err := env.ReadFile("/proc/sys/net/ipv4/ping_group_range")
	if err != nil {
		result.add(section, "ping_group_range", Warn, err.Error(), "Could not verify whether the kernel allows unprivileged ICMP for this process.")
		return
	}

	start, end, err := parsePingGroupRange(string(data))
	if err != nil {
		result.add(section, "ping_group_range", Warn, err.Error(), "Could not parse net.ipv4.ping_group_range.")
		return
	}

	gids := []int{env.Getgid()}
	groups, groupErr := env.Getgroups()
	if groupErr != nil {
		result.add(section, "supplementary groups", Warn, groupErr.Error(), "Checking primary GID only.")
	} else {
		gids = append(gids, groups...)
	}
	gids = uniqueInts(gids)

	if anyGIDInRange(gids, start, end) {
		result.add(section, "ping_group_range", Pass, fmt.Sprintf("%d %d includes gids %v", start, end, gids), "")
		return
	}
	result.add(section, "ping_group_range", Fail, fmt.Sprintf("%d %d does not include gids %v", start, end, gids), "Set net.ipv4.ping_group_range to include the process effective or supplementary GID.")
}

func checkMTU(result *Result, env Env) {
	if result.TargetCounts[config.ProbeTypeMTU] == 0 {
		result.add("MTU", "environment", Skip, "no mtu targets in config", "")
		return
	}
	if env.CheckMTUPingSocket == nil {
		result.add("MTU", "ping socket + PMTUDISC", Warn, "not supported on this platform", "MTU probes require a Linux runtime with ICMP ping socket support.")
		return
	}

	checkPingGroupRange(result, env, "MTU")

	if err := env.CheckMTUPingSocket(); err != nil {
		result.add("MTU", "ping socket + PMTUDISC", Fail, err.Error(), "Ensure net.ipv4.ping_group_range includes the process effective or supplementary GID.")
		return
	}
	result.add("MTU", "ping socket + PMTUDISC", Pass, "ok", "")
}

func checkDNS(result *Result, env Env, targets []config.TargetConfig) {
	dnsTargets, dnsWithoutCustomServer := countDNSTargets(targets)
	if dnsTargets == 0 {
		result.add("DNS", "resolver", Skip, "no dns targets in config", "")
		return
	}

	data, err := env.ReadFile("/etc/resolv.conf")
	if err != nil {
		severity := Warn
		hint := "All DNS targets configure dns_server; system resolver may be unused."
		if dnsWithoutCustomServer > 0 {
			severity = Fail
			hint = "DNS targets without probe_opts.dns_server need a working system resolver."
		}
		result.add("DNS", "resolv.conf", severity, err.Error(), hint)
		return
	}

	nameservers := parseNameservers(string(data))
	if len(nameservers) == 0 {
		severity := Warn
		hint := "All DNS targets configure dns_server; system resolver may be unused."
		if dnsWithoutCustomServer > 0 {
			severity = Fail
			hint = "Add nameservers to /etc/resolv.conf or set probe_opts.dns_server for DNS targets."
		}
		result.add("DNS", "resolv.conf nameservers", severity, "none found", hint)
		return
	}

	result.add("DNS", "resolv.conf nameservers", Pass, strings.Join(nameservers, ", "), "")
}

func listenTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = ln.Close()
	return nil
}

func parsePingGroupRange(raw string) (int, int, error) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("expected two integers, got %q", strings.TrimSpace(raw))
	}
	start, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start gid %q: %w", fields[0], err)
	}
	end, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid end gid %q: %w", fields[1], err)
	}
	return start, end, nil
}

func anyGIDInRange(gids []int, start, end int) bool {
	for _, gid := range gids {
		if gid >= start && gid <= end {
			return true
		}
	}
	return false
}

func parseNameservers(resolvConf string) []string {
	var nameservers []string
	for _, line := range strings.Split(resolvConf, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			nameservers = append(nameservers, fields[1])
		}
	}
	return nameservers
}

func countDNSTargets(targets []config.TargetConfig) (total int, withoutCustomServer int) {
	for _, target := range targets {
		if target.ProbeType != config.ProbeTypeDNS {
			continue
		}
		total++
		if target.ProbeOpts.DNSServer == "" {
			withoutCustomServer++
		}
	}
	return total, withoutCustomServer
}

func sortedProbeTypes(counts map[config.ProbeType]int) []config.ProbeType {
	types := make([]config.ProbeType, 0, len(counts))
	for probeType := range counts {
		types = append(types, probeType)
	}
	sort.Slice(types, func(i, j int) bool {
		return string(types[i]) < string(types[j])
	})
	return types
}

func uniqueInts(values []int) []int {
	seen := make(map[int]bool, len(values))
	unique := make([]int, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	sort.Ints(unique)
	return unique
}
