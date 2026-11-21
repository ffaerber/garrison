# Host configuration

Things that run on the host, not in containers: Sanoid and restic configs, systemd units.

| File | Installed as | Does |
| --- | --- | --- |
| `hosts.sh` | `/usr/local/lib/garrison/hosts.sh`, run by `garrison-hosts.service` at boot, before k3s and Docker | Puts back the registry's `/etc/hosts` entry if cloud-init or Multipass wrote the file anew |

| `buildkit.sh` | `/usr/local/lib/garrison/buildkit.sh`, run by `garrison-buildkit.service` | BuildKit for the user units' pipelines, privileged and therefore outside the cluster, in Docker, reachable only on the builds network |
