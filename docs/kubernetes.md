# Garrison on Kubernetes

Garrison runs on k3s, a complete Kubernetes in one binary. This page explains how Garrison uses Kubernetes, and how what it did on Docker Swarm maps to Kubernetes. It is meant for learning: each section names the files to read.

The move is in progress, on the branch `k3s`. The platform runs on Kubernetes. The agents' sandboxes, the apps and more machines follow, in that order (see "Still to move").

## The pieces

| Kubernetes object | What it is | In Garrison |
| --- | --- | --- |
| **Namespace** | A space for names and rights | `garrison` holds the platform. Units will get one each |
| **Pod** | One or more containers that run together, with one address | Every service of the platform, e.g. the pod of `gitea` |
| **Deployment** | Keeps a number of identical pods running, and replaces them on a change | Most of the platform. `strategy: Recreate` where only one may run at a time (Gitea, Postgres, the deployer) |
| **DaemonSet** | One pod on every machine | node-exporter (metrics) and Alloy (logs) |
| **Service** | A fixed name and address in front of pods, chosen by their labels | `postgres`, `gitea`, ...: pods reach each other as `<service>` in their namespace, or `<service>.<namespace>.svc.cluster.local` from anywhere |
| **Ingress** | A route from a public hostname to a Service | `git.<domain>` → `gitea`. Traefik reads them |
| **ConfigMap** | Files and settings | A stack's config files (`garrison.io/files`, below) |
| **Secret** | Like a ConfigMap, for secrets | The passwords the installer makes, mounted at `/run/secrets/<name>` |
| **ServiceAccount**, **Role**, **ClusterRole**, **…Binding** | Who in the cluster may do what with the Kubernetes API | The deployer may do everything; the gateway only reads pods and logs; Traefik reads Ingresses; Prometheus reads node metrics |
| **NetworkPolicy** | Which pods may reach which | Replaces Swarm's separate networks |

`kubectl` is the command-line client: `kubectl -n garrison get pods`, `kubectl -n garrison logs deploy/gitea`, `kubectl -n garrison describe pod <name>` (why a pod does not start), `kubectl -n garrison exec -it deploy/postgres -- sh`. On the machine, as root, it works as is.

## A system stack is a manifest

Each repo of `garrison` holds a `kubernetes.yaml`: Kubernetes objects, one YAML document each, separated by `---`. Example: `garrison/gitea/kubernetes.yaml`, which has a Service, an Ingress and a Deployment.

The deployer applies it (`control-plane/internal/system`). The installer does the same for the first stacks, with `garrison system DIR`.
1. `${DOMAIN}` and the other `${VAR}`s are filled in. `${CONFIG_HASH}` is a fingerprint of the stack's files. A pod template carries it as the annotation `garrison.io/config-hash`, so a changed config file means a changed template, and Kubernetes replaces the pods.
2. Every object, and every workload's pods, gets the label `garrison.io/stack: <stack>`.
3. A ConfigMap with the annotation `garrison.io/files: a.yml b.yml` gets those files of the repo as its data.
4. `hostPath` volumes below `/garrison/` are ZFS datasets. They are created first, so snapshots and quotas work as before.
5. `kubectl apply --prune --selector garrison.io/stack=<stack>` makes the cluster match the file. What the file no longer has is deleted.

`garrison system -render DIR` prints the result without applying it.

## Traefik and names

Traefik runs on the host's network, on ports 80 and 443 (`garrison/traefik/kubernetes.yaml`, `hostNetwork: true`). People reach it there. So do the host's containerd and Docker, which pull images from `git.<domain>` (`/etc/hosts` points that name at the machine).

Routes are standard **Ingress** objects. A middleware, such as the login check `sso`, is defined in `garrison/traefik/dynamic.yaml`, and an Ingress names it with the annotation `traefik.ingress.kubernetes.io/router.middlewares: sso@file`. The default certificate comes from the same file.

Inside the cluster, CoreDNS answers every name of the installation (`*.<domain>`) with Traefik's Service. That is the `coredns-custom` ConfigMap that `install.sh` writes. So pods use the same URLs as people: Keycloak at `https://keycloak.<domain>` for everyone.

## Secrets

The installer makes one Secret per secret, holding one key of the same name: `kubectl -n garrison create secret generic postgres-password --from-file=postgres-password=...`.

A pod mounts the ones it needs in a single **projected** volume at `/run/secrets`, so programs keep the paths they had on Swarm. There is one trap: Kubernetes mounts a pod's API token at `/var/run/secrets/kubernetes.io/serviceaccount`, and `/var/run` is `/run`, which is read-only inside our volume.
- A pod that does not use the API sets `automountServiceAccountToken: false`.
- A pod that does (Traefik, the deployer, the gateway) gets its token inside the same projected volume (`serviceAccountToken`, `kube-root-ca.crt`).

Images of `garrison/*` are in Gitea's registry, which only signed-in accounts read. Pods pull them with the Secret `registry-pull` (`imagePullSecrets`), a token that may only read packages (`garrison/gitea/configure.sh`).

## Metrics and logs

- The kubelet on each machine measures every container. Prometheus scrapes it through the API server: the job `kubelet` in `garrison/prometheus/prometheus.yml`.
- Those metrics name only `namespace`, `pod` and `container`. The deployer exports `garrison_pod_info{namespace, pod, unit, stack, service}` (`control-plane/internal/health`), and queries join with it: `... * on (namespace, pod) group_left (stack, service) garrison_pod_info`. The dashboards and the gateway's `service_usage` do so.
- The deployer's health metrics (`garrison_service_replicas_desired`, `_running`, `_failed_tasks`) come from the Kubernetes API, so the alerts stay as they were.
- Alloy, one per machine, ships pod logs to Loki with the labels `namespace`, `stack`, `service`, `pod` and `container` (`garrison/loki/config.alloy`).

## What still uses Docker

Docker stays on the machine for what has no Kubernetes way yet:
- **Gitea's Actions runners.** act_runner runs jobs as Docker containers. Those containers look names up in the cluster's DNS (`--dns 10.43.0.10`), where Gitea is `gitea.garrison.svc.cluster.local`.
- **BuildKit**, which builds the apps' images.
- **The agents' sandboxes**, until phase 2.

## Traps found

- **AppArmor on Ubuntu 26.04.** The runc that k3s ships (1.4.2) puts containers under the AppArmor label `cri-containerd.apparmor.d//&unconfined`, which the profile does not let signal its own processes: Postgres could not start, and `gosu` hung. Ubuntu's runc and crun apply the profile as it is. k3s runs pods with crun (`default-runtime: crun` in `/etc/rancher/k3s/config.yaml`).
- **A StatefulSet does not replace a pod that never became ready** when its template changes: it waits for it. One pod on a host directory needs no StatefulSet. Postgres is a Deployment with `strategy: Recreate`.
- **A new hostname is a new node.** k3s names its node after the hostname; a clone with a new hostname would register a second node, with every pod stuck on the old one. The installer pins `node-name`.
- **The service-account token and `/run/secrets`:** see Secrets above.

## Still to move

1. **Sandboxes as pods** under the RuntimeClass `gvisor`. The sandbox guard becomes RBAC and an admission policy.
2. **Apps**:
   - `internal/render` writes Deployments, Services and Ingresses instead of a Compose file;
   - one namespace per unit;
   - snapshots, restores, previews, secrets and the gate move with it.
3. **More machines**: `k3s agent` joins; app data moves to OpenEBS ZFS LocalPV volumes, or Longhorn where copies are wanted.
