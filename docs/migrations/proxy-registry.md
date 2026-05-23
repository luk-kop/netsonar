# Proxy Registry Migration for v0.8.0

Use this procedure when upgrading to NetSonar `v0.8.0`.

NetSonar `v0.8.0` replaces per-target `probe_opts.proxy_url` with a top-level
`proxies` registry and target-level `proxy_name`.

The change is breaking for existing proxy users. Direct targets that did not
use `probe_opts.proxy_url` do not need a proxy migration.

## Configuration

Before:

```yaml
targets:
  - name: ssm-eu-central-1
    address: https://ssm.eu-central-1.amazonaws.com
    probe_type: http
    probe_opts:
      proxy_url: "http://user:pass@infra-proxy.example.internal:8888"
```

After:

```yaml
proxies:
  infra-egress:
    url: "http://infra-proxy.example.internal:8888"
    username_env: "NETSONAR_PROXY_INFRA_USER"
    password_file: "/run/secrets/netsonar_proxy_infra_pass"

targets:
  - name: ssm-eu-central-1
    address: https://ssm.eu-central-1.amazonaws.com
    probe_type: http
    proxy_name: infra-egress
```

Migration steps:

1. Create one top-level `proxies.<name>` entry for each proxy endpoint.
2. Move the clean proxy endpoint to `proxies.<name>.url`.
3. Remove any URL userinfo from proxy URLs.
4. Configure proxy auth with `username`, `username_env`, or `username_file`,
   plus `password_env` or `password_file`.
5. Replace each target `probe_opts.proxy_url` with `proxy_name: <name>`.
6. Remove empty `proxy_url: ""` entries from direct targets.

## Proxy Auth

URL userinfo is rejected. Do not configure credentials as
`http://user:pass@proxy:port`.

Supported username sources:

- `username`
- `username_env`
- `username_file`

Supported password sources:

- `password_env`
- `password_file`

Inline `password` is not supported. `*_file` paths must be absolute.

Credential values from environment variables and files are resolved during
startup and reload attempts. Changing only the value behind the same env/file
source does not change the config hash and does not restart targets. A failed
credential read rejects reload and keeps the previous config active.

## TLS

`probe_opts.tls_skip_verify` applies only to target TLS verification.

For HTTPS proxy endpoints, use `proxies.<name>.tls_skip_verify` to control proxy
TLS verification. Setting `tls_skip_verify: true` on an `http://` proxy endpoint
is invalid.

## Environment Proxies

Probe traffic ignores `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`, and lowercase
variants. Proxy routing for probes is controlled only by NetSonar config.

## Metrics

Probe metrics replace the fixed `network_path` label with `proxy_name`.

Before:

```promql
probe_success{network_path="proxy"}
probe_success{network_path="direct"}
```

After:

```promql
probe_success{proxy_name!=""}
probe_success{proxy_name=""}
```

Use `netsonar_target_proxy_info` to map proxied targets to concrete proxy
endpoints:

```promql
netsonar_target_proxy_info{target_name="ssm-eu-central-1",proxy_name="infra-egress",proxy_endpoint="http://infra-proxy.example.internal:8888"} 1
```

Dashboards or ad-hoc queries that need endpoint visibility should join probe
metrics with `netsonar_target_proxy_info` on `target_name`. `proxy_endpoint` is
not added to `probe_*` metrics.
