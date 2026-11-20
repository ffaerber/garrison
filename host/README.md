# Host configuration

Things that run on the host, not in containers: Sanoid and restic configs, systemd units.

| File | Installed as | Does |
| --- | --- | --- |
| `repair-services.sh` | `/usr/local/lib/garrison/repair-services.sh`, run by `garrison-repair.service` at boot | At boot: puts back the registry's `/etc/hosts` entry if cloud-init removed it, and replaces Swarm tasks that Docker 29 restarted without their secrets or configs |

| `buildkit.sh` | `/usr/local/lib/garrison/buildkit.sh`, run by `garrison-buildkit.service` | BuildKit for the user units' pipelines, privileged and therefore outside Swarm, reachable only on the builds network |
