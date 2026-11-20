# Dashboards

Grafana dashboards as code. Any repo of a unit can hold dashboards in its `dashboards/` directory, as Grafana's JSON model. The deployer puts each unit's dashboards in a Grafana folder named after the unit and keeps them as they are on `main`: a commit adds, changes or removes a dashboard. Changes made in Grafana's editor are overwritten; to keep one, export it (Share → Export → Export for sharing externally off) and commit the JSON here.

Each dashboard needs a `uid` that is unique in the installation; without one, the deployer derives it from the repo and file name. Use the data sources `prometheus` and `loki` by uid.

| File | Shows |
| --- | --- |
| `overview.json` | The machine and every stack at a glance, with recent errors |
| `stack.json` | One stack: its services' CPU, memory, network, restarts, requests and logs |
| `logs.json` | Every container's logs by stack and service, with search |
| `machine.json` | The host: CPU, memory, disks, ZFS, network, temperatures |
| `traffic.json` | Requests through Traefik by hostname and status, latency, certificate expiry |
