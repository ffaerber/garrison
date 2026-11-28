# More machines

A Garrison installation starts on one machine, the **main** one. Other machines join it to give the apps and the agents' sandboxes more room, each with a ZFS pool of its own. The platform (Gitea, Keycloak, Traefik, the control plane, the monitoring, the pipelines) stays on the main machine. How it works inside: [kubernetes.md](kubernetes.md), "More machines".

## Joining a machine

The new machine runs Ubuntu 26.04 and reaches the main one on ports 443 (the registry, through Traefik) and 6443 (Kubernetes), and the machines reach each other for the pod network (flannel's VXLAN, UDP 8472).

1. On the main machine, ask for the command:

   ```
   sudo ./install.sh --join-command
   ```

   It prints `sudo ./install.sh --join ADDRESS --domain DOMAIN --token TOKEN --ca-sha256 FINGERPRINT`. The token lets a machine into the cluster: treat it as a password.
2. On the new machine, with this repository checked out, run that command. Add `--pool-device /dev/sdX` to make its pool on a disk of its own (otherwise a sparse file of `--pool-size`, 20G by default, on the root disk).

The script (`installer/join.sh`):
- checks the certificate of the main machine against the fingerprint;
- installs ZFS, gVisor and crun;
- points `git.<domain>` at the main machine and trusts its certificate, for pulling images;
- makes the pool and has Sanoid snapshot it;
- joins as a k3s agent.

Running it again changes only what differs. On the main machine, `kubectl get nodes` lists the new machine; once it is Ready, it takes work.

## What runs where

| What | Where |
| --- | --- |
| The platform's services | The main machine (their Deployments name it, `system.Render`) |
| An app's services with data | The machine that holds its data |
| An app's services without data | Any machine |
| An agent's sandbox | Any machine; with its unit's media library, that library's machine. Its workspace stays where its first pod ran |
| The zfs pod, OpenEBS's agent, node-exporter, Alloy, the sandbox image | Every machine |

**Where an app's data goes.** When an environment is first deployed with data, the deployer picks its machine:
1. the machine of a share the stack mounts;
2. otherwise the machine whose pool has the most free space.

The environment's volumes stay on that machine. Its datasets are at `garrison/ns/<unit>/stacks/<stack>/<stack>.<dataset>` in that machine's pool, a share's at `garrison/ns/<unit>/shares/<unit>.shares.<share>`. Before a redeploy, the deployer snapshots them there. A restore copies a snapshot back there.

## Limits

- **Data moves on request.** An app's data stays on the machine that holds it; if that machine is down, so is the app (there are no copies on other machines yet). A request in the unit's config moves an environment with its data: `moves/<name>.yaml` with `stack`, `environment` (main by default) and `to:` the machine's name. Once merged, the deployer stops the environment, sends its datasets to that machine (`zfs send -R`, with their properties and snapshots), points its volumes there and starts it there; the old copy stays on the old machine as `<dataset>.moved-<time>`, a way back, until someone destroys it. The feed records it. This first version stops the app for the whole copy and streams through the Kubernetes API, which suits small data; an environment that mounts a unit's share cannot move alone.
- **A sandbox's workspace stays where it was made.** A chat whose workspace is on one machine cannot also mount a media library on another.
- **The main machine runs the platform.** If it is down, nothing is deployed and nobody signs in, though apps elsewhere keep running.
- **Leaving.** A machine leaves with `kubectl drain <node> --ignore-daemonsets` and `kubectl delete node <node>` on the main one, and `/usr/local/bin/k3s-agent-uninstall.sh` on it. Its apps' data stays in its pool: move them first (`moves/`, above).
