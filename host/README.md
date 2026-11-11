# Host configuration

Things that run on the host, not in containers: Sanoid and restic configs, systemd units.

| File | Installed as | Does |
| --- | --- | --- |
| `repair-services.sh` | `/usr/local/lib/homecloud/repair-services.sh`, run by `homecloud-repair.service` at boot | At boot: puts back the registry's `/etc/hosts` entry if cloud-init removed it, and replaces Swarm tasks that Docker 29 restarted without their secrets or configs |

| `buildkit.sh` | `/usr/local/lib/homecloud/buildkit.sh`, run by `homecloud-buildkit.service` | BuildKit for the user namespaces' pipelines, privileged and therefore outside Swarm, reachable only on the builds network |
