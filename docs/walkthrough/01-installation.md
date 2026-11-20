# 1. Installation

Garrison installs on a fresh Ubuntu machine with one command. It sets up
Docker Swarm, a ZFS pool, and the platform: Keycloak for signing in, Gitea
for code and config, Open WebUI for the chat with the agents, Grafana, and
Garrison's control plane. After that the control plane deploys everything
from Git, the platform included.

## What you need

- A machine with Ubuntu, and root on it.
- A domain whose names all point at the machine: `garrison.test`,
  `git.garrison.test`, `chat.garrison.test`, … (a wildcard DNS entry).
- An LLM the agents use: any OpenAI-compatible API, such as Ollama on your
  own computer or Ollama's cloud with an API key.

## An answers file

The installer asks nothing yet: it reads its answers from a file. This is
the one the tests use (`test/answers/selfsigned.env`):

```sh
DOMAIN=garrison.test
TLS_MODE=selfsigned            # a self-signed wildcard certificate
ADMIN_USER=admin
ADMIN_EMAIL=admin@garrison.test
POOL_SIZE=10G                  # the ZFS pool, in a file if there is no drive for it
REGISTRY_MIRROR=https://mirror.gcr.io
```

The LLM goes in the same file:

```sh
LLM_BASE_URL=http://192.168.1.10:11434/v1   # e.g. Ollama on another computer
LLM_MODEL=qwen3.8-64k:latest
# or Ollama's cloud: LLM_BASE_URL=https://ollama.com/v1 and LLM_API_KEY=…
```

## Running it

From a checkout of this repository, on the machine:

```sh
make build                                   # the garrison binary, built in Docker
sudo ./install.sh --answers test/answers/selfsigned.env
```

It takes about five minutes, and can be run again: every step checks what
is already there. What it prints, from a test run:

```text
==> [0m00s] Installing Docker and ZFS
==> [0m09s] Installing gVisor
==> [0m18s] Setting up Docker Swarm
==> [0m19s] Setting up the ZFS pool
==> [0m19s] Generating secrets
==> [0m22s] Deploying postgres
==> [0m34s] Deploying traefik
==> [0m43s] Deploying keycloak
==> [1m32s] Deploying sso
==> [1m38s] Deploying gitea
==> [1m55s] Deploying runner
==> [1m58s] Building the control plane in Gitea
==> [2m49s] Handing over to the deployer
deployed gitea at e0aa1fd96b6c
deployed grafana at d6066352db80
deployed keycloak at f0a2bbfc5b39
…
deployed control-plane at a9e63ba6abd0
applied 11 stacks
  the deployer runs git.garrison.test/garrison/control-plane:a15a21aa7163

==> [4m56s] Done
  Keycloak  https://keycloak.garrison.test  (people sign in to the realm "garrison")
  Gitea     https://git.garrison.test
  Chat      https://chat.garrison.test  (one agent per unit)
  Grafana   https://grafana.garrison.test
  Admin     admin (password in /etc/garrison/admin-password)
  TLS       self-signed; certificate in /etc/garrison/tls.crt

  Recovery key for every secret. Keep it offline, e.g. printed or in a
  password manager; it is not kept on this machine and is not shown again.
  …
```

Keep the recovery key it shows at the end: with it, any secret in a config
repo can be read even if the machine is lost. It is shown only once.

## People

The admin adds people in Keycloak (`https://keycloak.garrison.test`, realm
`garrison`). On a test installation, `test/e2e/add-test-users.sh` adds alice,
bob and carol, with the password `12345678`.

## Trying it on a VM

`test/vm/steps.sh` does all of this on a throwaway Multipass VM, then runs
the tests. After a green run, this computer's DNS points `garrison.test` at
the VM (`test/vm/dns.sh`), so every page opens in your browser. The browser
will warn about the self-signed certificate.

Next: [2. Signing in](02-signing-in.md)
