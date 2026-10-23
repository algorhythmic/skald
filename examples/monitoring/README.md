# Homelab monitoring drafts

These files target **192.168.86.21**. Nothing installs or starts a monitoring
service on the workstation. The existing Prometheus 3.13.3 remote-write receiver
on that host is already enabled; preserve its current configuration and data.

Install an OpenTelemetry **Collector Contrib** binary at
`/usr/local/bin/otelcol-contrib` on `.21`. The draft was validated offline with
Contrib 0.163.2. Copy `otelcol-contrib.homelab.yml` to
`/etc/otelcol-contrib/skald.yml` and `skald-otelcol.homelab.service` to
`/etc/systemd/system/skald-otelcol.service`. On that host, review and run:

```sh
/usr/local/bin/otelcol-contrib validate --config=/etc/otelcol-contrib/skald.yml
sudo systemctl daemon-reload
sudo systemctl enable --now skald-otelcol.service
```

The HTTP receiver binds `.21:4318` and forwards to
`http://127.0.0.1:9090/api/v1/write` on the same host. Permit the workstation's
access to port 4318 on your private network. Copy `alerts.yml` to
`/etc/prometheus/skald-alerts.yml`, merge `prometheus-rules.fragment.yml` into the
existing Prometheus configuration, validate it with that host's `promtool`, and
reload the existing service. No outbound alert destination is supplied.

After installation is verified, the future Skald configuration is:

```json
{
  "telemetry": {
    "otlp_metrics_endpoint": "http://192.168.86.21:4318/v1/metrics",
    "service_instance_id": "workstation-01",
    "export_interval_seconds": 30
  }
}
```

The live endpoint is currently **unset**. Choose a distinct nonsecret device
alias for each device; avoid hostnames and paths. The resource attributes
`service.name` and `service.instance.id` become Prometheus series labels, keeping
device measurements distinct. `dashboard-links.json` supplies graph links.

Run `python3 scripts/monitoring_validate.py` from the repository for offline
collector/rule validation. It never opens a listener or contacts the homelab.
