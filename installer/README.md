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

The installer keeps its state in `/etc/homecloud` (root only): the admin password, the TLS certificate and key, and `install.env`.
