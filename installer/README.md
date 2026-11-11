# Installer

Bootstrap Compose file and setup wizard, with an answers-file mode for headless installs.

Only the headless mode exists, in `/install.sh`:

```
sudo ./install.sh --answers test/answers/selfsigned.env
```

Answers file keys (shell `KEY=value`):

| Key | Meaning | Default |
| --- | --- | --- |
| `DOMAIN` | Installation domain | required |
| `TLS_MODE` | `selfsigned`; `letsencrypt` is not built | `selfsigned` |
| `ADMIN_USER`, `ADMIN_EMAIL` | First admin account | `admin`, `admin@<domain>` |
| `ADMIN_PASSWORD` | Admin password | generated |
| `POOL_DEVICE` | Device for the ZFS pool | a sparse file on the root disk |
| `POOL_SIZE` | Size of that sparse file | `20G` |
| `LLM_BASE_URL`, `LLM_MODEL` | OpenAI-compatible API and model of the namespace agents in Open WebUI | `https://ollama.com/v1`, `gpt-oss:120b` |
| `LLM_API_KEY` | Its key; stored root-only in `/etc/homecloud/llm-api-key`, which may also be filled by hand | none: the agents cannot answer |
| `REGISTRY_MIRROR` | Pull-through mirror for Docker Hub images, e.g. `https://mirror.gcr.io` | none |

The installer keeps its state in `/etc/homecloud` (root only): the admin password, the TLS certificate and key, the build token, and `install.env`.

| Script | Does |
| --- | --- |
| `push-repo.sh` | Creates a repo in Gitea and fills it from a directory; `--update` commits changes to an existing one |
| `controller-tree.sh` | Assembles the repo `system/controller`: its stack, the source, Dockerfile and build workflow |
| `lib.sh` | Helpers for the `configure.sh` steps of the bootstrap stacks |
