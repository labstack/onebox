# Coverage survey

276 Compose files from popular GitHub repositories, 1,367 services, none
unparsable. Fetched by repository search across `docker-compose`, `self-hosted`,
`homelab` and related topics, then reduced to files declaring services.

The question was not whether the contract is elegant but how much of what people
actually write it can express without the escape hatch.

## Result

| | Services | Projects |
|---|---|---|
| Before | 47% | 34% |
| After | **66%** | **54%** |

## What changed

The survey ranked every Compose key by how many projects use it, and the top of
the list was not exotic. Nine fields carrying no new concept — scalars, bools
and flat lists — stood between two thirds of services and the declaration:

`entrypoint`, `user`, `hostname`, `labels`, `working_dir`, `init`, `tty`,
`stdin_open`, `extra_hosts`.

Each was measured before being added, cumulatively:

| Added | Services | Projects |
|---|---|---|
| entrypoint | 51% | 37% |
| user | 53% | 40% |
| hostname | 56% | 43% |
| labels | 62% | 45% |
| working_dir | 63% | 47% |
| init | 63% | 47% |
| tty | 64% | 49% |
| stdin_open | 64% | 51% |
| extra_hosts | 66% | 54% |

Labels are the only one with semantics: the user's land first, Onebox's follow,
and the schema reserves the `ob.` and `traefik.` namespaces so nothing a user
wrote can be silently overwritten.

## What deliberately remains outside

Ranked by how many projects would still need a Compose reference:

| Key | Projects | Why it stays out |
|---|---|---|
| `logging` | 11% | Onebox owns log rotation. Per-service logging drivers are a platform decision it should make uniformly, not a per-workload declaration |
| `deploy` | 10% | Its useful parts — replicas, resource limits — are already expressible; the rest is orchestrator-specific |
| `cap_add`, `cap_drop`, `security_opt`, `privileged`, `devices`, `shm_size`, `tmpfs`, `ulimits`, `sysctls`, `read_only` | 3–9% each | The hardening and hardware family, named in the contract as escape-hatch territory. Real but rare, runtime-specific, and modelling them would grow the contract far more than it would help |
| `network_mode`, `links` | 6–7% | Legacy networking, superseded by the generated network and declared prerequisites |
| `secrets` | 3% | Compose's own secret mechanism; the contract has its own |

A workload using any of these is still fully operated: released, health-gated,
routed, and rolled back like any other. It is authored in Compose and
referenced, which is what the escape hatch is for.

## Honest reading

Roughly half of real projects still need a Compose reference for at least one
service. That is the design working rather than failing — but it means the
ten-line project file is the shape of a *simple* application, not of every
application, and the documentation should say so.
