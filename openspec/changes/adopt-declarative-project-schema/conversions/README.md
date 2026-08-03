# Conversion drafts

Task 1.1–1.3. Every project below was expressed under `schema.cue` and validated
against it. The four adopting projects and `fanout` are the stated acceptance
test; the five external stacks were added to check the contract against code
nobody here wrote.

| Draft | Source | What it stresses |
|---|---|---|
| `goal` | this organization | rolling application, two data services, local bootstrap hook |
| `monk` | this organization | exec health check, worker, ofelia cron, local pre-release and post-deploy hooks, advisory verification with `contains` |
| `pursue` | this organization | migration job, pinned proxy image, registry credentials |
| `recast` | this organization | migration job, ClamAV daemon, runner and plan-schema policy |
| `fanout` | this organization, **declined the previous schema** | three sites on one host, four hostnames, OTLP gRPC listener, three per-service env files, proxy config staging |
| `ext-umami` | umami-software/umami | smallest real stack: app plus one database, health-gated |
| `ext-paperless` | paperless-ngx/paperless-ngx | five services, published host port, single-workload env file |
| `ext-n8n` | n8n-io/n8n-hosting | queue-mode worker split from one image, sidecar runners, staged init script |
| `ext-plausible` | plausible/community-edition | two unlike datastores, four mounted XML config files, ulimits |
| `ext-immich` | immich-app/immich | ML sidecar, `shm_size`, one env file shared by two of four workloads |

## What the exercise changed

Structural validation passed early and proved little. The value was in what had
to be worked around, and five gaps were confirmed by more than one project each:

1. **`#Health` variants were not discriminated.** The first draft of `goal` failed
   immediately. The 57-case corpus had missed it because it only ever tested the
   scalar form. Fixed by mutual negation, as verification already had.
2. **Environment files were project-wide only.** paperless gives one file to its
   web server alone, immich shares one between two of four services, fanout has
   three for three services. A project-wide list would have put every service's
   secrets in every container. Now per-workload, with the project list as a
   default for applications and workers.
3. **`needs` could not express health-gating.** All ten projects gate startup on a
   dependency being *healthy*, not merely started. A prerequisite now carries a
   condition, defaulting to `healthy`.
4. **Host ports had no home.** paperless publishes `8000:8000`, outline binds
   `127.0.0.1:5432`. Not everything is reached through the proxy. Published ports
   now exist and bind to loopback unless stated otherwise.
5. **Mounted configuration was never staged.** plausible mounts four ClickHouse
   XML files, n8n an init script, fanout its proxy config. The Compose reference
   was staged but the files it mounts were not. A `files:` list now stages them.

`fanout` additionally needed a backend scheme: its OTLP listener is gRPC, which
`protocol` and TLS mode alone cannot express. Routes now carry `scheme`.

## Known conversion costs

- `recast` declares `minimum_onebox_version: 0.0.1-m0`, which is not a release
  identity. It must become a real CalVer or be removed.
- `monk` sets `container_name: feed`; container naming is owned by Onebox, so the
  key is deleted during conversion.
- Image references are pinned by hand in these drafts. Resolving them from a
  version is the release-pipeline change, not this one.
- `monk`'s ofelia cron jobs stay as a daemon here. They become scheduled jobs
  once `schedule` is implemented, which is what retires ofelia.
