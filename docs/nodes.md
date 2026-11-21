# More machines

Garrison is installed on one machine, the **main machine**. Other machines can join it and run apps. The first part of this is built: machines without data. The rest is planned.

## What runs where

- **The main machine** is the swarm's manager. It runs the whole platform: Gitea and its registry, Postgres, Keycloak, Traefik, Open WebUI, the control plane, the agents' sandboxes, BuildKit and the runners. Every service of `garrison/*/compose.yaml` says `node.role == manager`, except the collectors that run on every machine (`mode: global`): node-exporter, cAdvisor and Alloy. `TestThePlatformStaysOnTheMainMachine` (`control-plane/internal/system`) fails for a service that says neither.
- **Apps with data** run on the main machine too. Their datasets and the unit's shares are ZFS datasets there, so the renderer gives every service with a volume the constraint `node.role == manager` (`render.DataNode`).
- **Apps without data** may run on any machine. Swarm puts a new task on the machine running the fewest, and Traefik on the main machine routes to it over the overlay network `traefik-public`.

An app's parts can be split this way. For example, todo's `web` may run on another machine while its `db` stays on the main one.

## Adding a machine

On the main machine:

```
sudo ./install.sh --join-command
```

It prints the command to run on the new machine, an Ubuntu machine with this repository checked out:

```
sudo ./install.sh --join 10.0.0.5 --domain example.com --token SWMTKN-... --ca-sha256 AB:CD:... [--mirror URL]
```

`installer/join.sh` does the work:

1. It installs Docker.
2. It sets the main machine's Docker Hub mirror, if it has one.
3. It points `git.<domain>` at the main machine, so Docker pulls app images from Gitea's registry through Traefik there. Swarm hands the deployer's registry login to each task (`--with-registry-auth`).
4. It fetches the installation's certificate from that address and trusts it only if its fingerprint matches.
5. It joins the swarm as a worker.

Running it again is safe.

The machines must reach each other on Swarm's ports: 2377/tcp to the main machine, 7946/tcp and udp, and 4789/udp. The new machine must also reach the main machine on 443.

Apps that are running stay where they are. Only new tasks go to the new machine, for example after a deploy or a restart.

## Taking a machine out

On the main machine:

1. `docker node update --availability drain <node>`. Its tasks move to the other machines.
2. `docker swarm leave` on the machine itself.
3. `docker node rm <node>` on the main machine.

## Tested

The scenario `nodes` (`test/e2e/scenarios/nodes.sh`, from the fixture `deploy`) checks this on two VMs:

- a clone of the base VM joins todo's main machine with the printed command;
- only the collectors run on it;
- todo's web part, restarted, moves to it and pulls its image from the registry;
- the database stays on the main machine, also when it restarts;
- alice's todos still load;
- once the machine is drained, the web part is back on the main machine.

## Not built yet

- **Apps with data on other machines.** Planned design:
  - each environment gets a home machine, recorded in Git;
  - the renderer pins the environment to that machine;
  - a small service on every machine does that machine's ZFS work (datasets, quotas, snapshots, restores) for the deployer;
  - Sanoid runs on every machine;
  - an app moves between machines with `zfs send | zfs recv` and a pull request that changes its home.
- **GPU machines for apps.** A service with `devices: [gpu]` reserves a GPU, which only machines that advertise one offer. Joining does not set that up yet.
- **Repair after a restart.** After Docker restarts on a joined machine, its containers can come back without their secrets. The main machine's `garrison-repair.service` replaces only its own. Until a joined machine repairs its own, restart its apps from the main machine: `docker service update --force <service>`.
- **The main machine is a single point of failure.** If it is down, so are the platform and every app, since Traefik runs only there.
