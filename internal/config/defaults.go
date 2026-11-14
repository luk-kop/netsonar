package config

import "slices"

// applyDefaults fills in missing target-level fields from agent-level defaults.
func applyDefaults(cfg *Config) {
	if cfg.Agent.ListenAddr == "" {
		cfg.Agent.ListenAddr = ":9275"
	}
	if cfg.Agent.MetricsPath == "" {
		cfg.Agent.MetricsPath = "/metrics"
	}
	if cfg.Agent.LogLevel == "" {
		cfg.Agent.LogLevel = "info"
	}
	if cfg.Agent.LogFormat == "" {
		cfg.Agent.LogFormat = "text"
	}

	for i := range cfg.Targets {
		if cfg.Targets[i].Interval == 0 {
			cfg.Targets[i].Interval = cfg.Agent.DefaultInterval
		}
		if cfg.Targets[i].Timeout == 0 {
			cfg.Targets[i].Timeout = cfg.Agent.DefaultTimeout
		}

		// Propagate agent.dns_resolver only when the target did not specify
		// the field at all. An explicit empty string in the target YAML
		// (`dns_resolver: ""`) is a deliberate opt-out from the agent
		// default and must be preserved as a non-nil empty pointer.
		if cfg.Targets[i].DNSResolver == nil {
			v := cfg.Agent.DNSResolver
			cfg.Targets[i].DNSResolver = &v
		}

		// Apply default ICMP payload sizes for MTU probes that don't specify their own.
		if cfg.Targets[i].ProbeType == ProbeTypeMTU && len(cfg.Targets[i].ProbeOpts.ICMPPayloadSizes) == 0 {
			if len(cfg.Agent.DefaultICMPPayloadSizes) > 0 {
				cfg.Targets[i].ProbeOpts.ICMPPayloadSizes = slices.Clone(cfg.Agent.DefaultICMPPayloadSizes)
			} else {
				cfg.Targets[i].ProbeOpts.ICMPPayloadSizes = slices.Clone(DefaultICMPPayloadSizes)
			}
		}
		if cfg.Targets[i].ProbeType == ProbeTypeMTU {
			opts := &cfg.Targets[i].ProbeOpts
			if opts.MTURetries == 0 {
				opts.MTURetries = DefaultMTURetries
			}
			if opts.MTUPerAttemptTimeout == 0 {
				opts.MTUPerAttemptTimeout = DefaultMTUPerAttemptTimeout
			}
			if opts.ExpectedMinMTU == 0 && len(opts.ICMPPayloadSizes) > 0 {
				opts.ExpectedMinMTU = opts.ICMPPayloadSizes[0] + 28
			}
		}
	}
}
