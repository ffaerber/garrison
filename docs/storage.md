# Storage: disks, classes, copies and moves (design)

A proposal, not built yet. How storage works now: [kubernetes.md](kubernetes.md) ("More machines") and Snapshots and restore in [status.md](status.md).

## Now

- Each machine has one ZFS pool, `garrison`: on `POOL_DEVICE`, or a file on the system disk. Every dataset lives in it: each environment's volumes (`garrison/ns/<unit>/stacks/<stack>/<stack>.<volume>`, through OpenEBS ZFS LocalPV), the units' shares, the platform's data.
- Snapshots: before each redeploy (`deploy-…`), on a schedule by Sanoid (`autosnap_…`), before a restore (`restore-…`), on request (`manual-…`). A restore, requested in Git, copies an environment's data back from one.
- An app's data stays on the machine that holds it. Snapshots are on the same disk as the data: they help against mistakes and bad deploys, not against a disk that dies.

## Pools by class

Which pools exist, on which machine, on which disks, and of what kind, is a file in Git, `garrison/storage/pools.yaml`, changed by root admins:

```yaml
pools:
  - machine: main
    name: fast
    class: fast              # NVMe: databases, apps
    layout: mirror           # two disks, either may fail
    disks: [nvme0n1, nvme1n1]
  - machine: main
    name: bulk
    class: bulk              # hard disks: media, files, large data
    layout: raidz1
    disks: [sda, sdb, sdc]
  - machine: main
    name: backup
    class: backup            # only receives copies
    disks: [sdd]
```

- Making a pool erases its disks. The dashboard's Storage page proposes the change to `pools.yaml`; a root admin must also confirm each disk by its name before anything is erased. No agent does it, whatever its grants.
- The installer's one pool stays as it is, of class `fast`; an installation with one disk needs nothing more.

## Volumes choose a class

A stack's volume says what it needs, not where:

```yaml
volumes:
  data:  { path: /var/lib/postgresql/data, size: 2Gi }       # class fast, the default
  media: { path: /srv/media, size: 500Gi, class: bulk }
```

- The deployer puts a new volume on a machine with a pool of its class, as it chooses a machine now (the environment's other volumes, a share it mounts, the most free space).
- A share's class is `bulk` by default.
- A volume stays where it is when its class changes in the file: it moves only by a move (below).

## Copies and backups

What is copied where, how often, and how long it is kept: `garrison/storage/backup.yaml`, per class or unit.

```yaml
copies:                     # to another disk or machine, with zfs send
  - what: { class: fast }
    to: { pool: backup }    # or { machine: second, pool: backup }
    every: 1h
    keep: { hourly: 24, daily: 30 }
offsite:                    # encrypted, to another place
  - what: { class: fast }
    to: s3://bucket/garrison   # its key: a secret of garrison
    every: 1d
    keep: { daily: 30, monthly: 12 }
```

- **Copies** send snapshots with `zfs send`, incremental after the first (`-i`), resumable (`-t`): a dead data disk leaves the copy.
- **Off-site** with restic, encrypted, over HTTPS: a machine that burns leaves the copy. The key is a secret of garrison, as the LLM providers' are.
- The feed records each run; the Storage page says when each last succeeded, and an alert fires when one is late.

## How data travels between machines

Without SSH. Each machine runs Garrison's privileged `zfs` pod (a DaemonSet), through which the deployer runs every ZFS command there.

- For a copy or a move, the receiving machine's `zfs` pod opens a port for this one transfer, with a one-time token; the sending machine's streams `zfs send` straight to it over the cluster's network. The deployer runs both sides.
- k3s's network between machines is not encrypted by default (flannel with VXLAN): the machines' traffic goes through WireGuard (`--flannel-backend=wireguard-native`), which the installer and `join.sh` set up.
- Small amounts could go through the Kubernetes API (`kubectl exec … zfs send | kubectl exec … zfs receive`), but every byte would pass the main machine; it is not used for data of any size.
- SSH (as syncoid uses it) would need keys between every two machines and an open `sshd`: one more door, where the machines are joined already.

## Moving an app to another machine

A request in Git, like a restore: `moves/<name>.yaml` in the unit's config (`stack`, `environment`, `to: <machine>`), merged by an admin, as a move stops the app for a moment. The deployer then:

| Step | What | The app |
| --- | --- | --- |
| 1 | A snapshot (`move-…`), copied in full to the target machine | runs on the old machine |
| 2 | Further snapshots, copied incrementally, until what is left is small | runs |
| 3 | Stops the environment on the old machine | down |
| 4 | A last snapshot, copied incrementally: seconds | down |
| 5 | Points the volumes (ZFSVolume, PersistentVolume) at the target machine and starts the environment there | starting |
| 6 | Waits for it to be healthy | runs on the new machine |
| 7 | Keeps the old copy, read-only, for a while (`keep`), as a way back; then destroys it | runs |

- If step 5 or 6 fails, the environment starts again on the old machine, whose data has not changed since step 3: nothing is lost.
- The app runs on one machine at a time: it stops before the last copy, never after the start elsewhere, so no write is lost.
- An environment that mounts a share moves only with the share and every other stack that mounts it; the deployer refuses a part of them, and names the others.
- The feed records each step ("copying 12 GB to second", "todo stopped", "todo runs on second, down 9 s").

## The Storage page

On the dashboard, for root admins: the disks each machine has, its pools with their health (`zpool status`), use and last scrub, the disks' SMART health, and the copies and off-site backups with their last run. From it, a new pool or a disk added is proposed as a change to `pools.yaml`, never made directly.

## Order

1. Pools by class in `pools.yaml`, volumes by class, the deployer placing them.
2. Transfers between the machines' `zfs` pods, WireGuard between machines; copies to a backup pool or another machine.
3. Moves (`moves/<name>.yaml`).
4. Off-site backups with restic.
5. The Storage page.
