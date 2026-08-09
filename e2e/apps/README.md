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

## Deployed for real — one application, one host

`one-app-one-host.sh` provisions a throwaway Hetzner VM, deploys one
application to it from a bare Ubuntu image with `ob bootstrap` then `ob deploy`, verifies it
serves, and destroys the host. One application per host, which is the product's
scope — putting several on one box would not test what this contract
describes.

| App | Bare image to serving | HTTP | Containers | Volumes |
|---|---|---|---|---|
| vaultwarden | 39s | 200 | 1 | 1 |
| gitea | 48s | 200 | 2 | 2 |
| uptime-kuma | 53s | 302 | 1 | 0 |
| umami | 58s | 200 | 2 | 1 |
| n8n | 63s | 200 | 4 | 4 |
| paperless | 78s | 302 | 5 | 4 |

Every host was destroyed at the end of its run, including on failure.

Three defects were found by doing it, none of which local testing had caught:

1. **Compose renamed our volumes.** It prefixes the project name unless the name
   is pinned, so the volume Docker created was not the one the naming contract
   promises — and preflight, which looks for the contract name, would never have
   seen a collision that existed.
2. **Volumes carried no ownership.** Preflight tells a previous release from a
   stranger's resource by label, and an unlabelled volume that Onebox itself
   created looked like a foreign collision.
3. **`needs` defaulted to an impossible condition.** Waiting for a dependency to
   become healthy is unsatisfiable when that dependency declares no health
   check, and the container engine refuses to start the runtime at all:
   *"dependency failed to start: container has no healthcheck configured."* The
   condition is now resolved against what the dependency can actually offer.
