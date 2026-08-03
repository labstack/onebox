# Deployable application fixtures

Self-contained `onebox.run/v1` projects for real open-source applications,
chosen for the shape people normally build rather than for being exotic. Each
declares everything it needs, so it renders and runs without a Compose
reference.

Every one was rendered with `ob preview`, validated by `docker compose config`,
and — where marked — actually started locally and served traffic.

| Project | Workloads | Proven |
|---|---|---|
| `uptime-kuma` | 1 | rendered, valid, ran, healthy |
| `vaultwarden` | 1 | rendered, valid, ran, healthy |
| `umami` | app + Postgres | rendered, valid, ran, healthy, served `/api/heartbeat` |
| `gitea` | app + Postgres, SSH on a published port | rendered, valid, ran, served HTTP 200 |
| `n8n` | app + worker + Postgres + Redis | rendered, valid |
| `paperless` | app + Valkey + Postgres + Gotenberg + Tika | rendered, valid |

`server: root@TARGET` is a placeholder. Point it at a host before using one.

To try one:

```sh
ob preview -c e2e/apps/umami.yml            # what would run
ob preflight -c e2e/apps/umami.yml          # whether the host is ready
```
