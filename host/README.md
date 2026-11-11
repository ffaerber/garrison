# Host configuration

Things that run on the host, not in containers: Sanoid and restic configs, systemd units.

| File | Installed as | Does |
| --- | --- | --- |
| `repair-services.sh` | `/usr/local/lib/homecloud/repair-services.sh`, run by `homecloud-repair.service` at boot | Replaces Swarm tasks that use secrets or configs, because Docker 29 restarts them without those mounts after a daemon restart |

The controller's unit, `homecloud-controller.service`, is written by the installer.
