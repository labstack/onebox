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
| `deploy` | 10% | Orchestrator-specific. Measured: **none** of its 93 uses were limited to replicas and resources, so the assumption that its useful parts were already covered was wrong |
| `links` | 6% | Legacy, superseded by the generated network and declared prerequisites |
| `extends` | 6% | Not followed, and now **refused** rather than silently rendered without what it inherits |
| `cap_add`, `cap_drop`, `security_opt`, `privileged`, `devices`, `shm_size`, `tmpfs`, `ulimits`, `sysctls`, `read_only` | 3–9% each | The hardening and hardware family, named in the contract as escape-hatch territory. Real but rare, runtime-specific, and modelling them would grow the contract far more than it would help |
| `network_mode` | 7% | Legacy networking, superseded by the generated network |
| `secrets` | 3% | Compose's own secret mechanism; the contract has its own |

A workload using any of these is still fully operated: released, health-gated,
routed, and rolled back like any other. It is authored in Compose and
referenced, which is what the escape hatch is for.

## Honest reading

Roughly half of real projects still need a Compose reference for at least one
service. That is the design working rather than failing — but it means the
ten-line project file is the shape of a *simple* application, not of every
application, and the documentation should say so.


## A second pass: why services actually fail

Ranking keys is not the same as knowing why a service fails, so the failures
were opened up.

**68% of failing services fail on exactly one key.** The tail is thin: only 4% use
four or more unsupported keys. That is what made widening cheap.

`logging` alone blocked 95 services as their *only* unsupported key — more than
anything else — so it was added as a passthrough. Onebox still owns log
rotation; which driver a workload logs through is a choice people demonstrably
make, and overriding it silently would be wrong.

Three findings were not coverage gaps at all but defects, where the runtime
would have been wrong rather than refused:

1. **`extends` was not followed.** The referenced file is read as plain YAML on
   purpose, so referencing one service cannot fail on another's missing
   variable. But a service using `extends` would then render without what it
   inherits. It is now refused by name.
2. **Custom networks were dropped.** 85 of the 97 projects declaring top-level
   networks use non-default topology. A service isolated on a backend network
   would have landed on the default one.
3. **Volume driver options were dropped.** 35 projects declare them. An
   NFS-backed volume would have become a local directory, and nobody would have
   noticed until the data was in the wrong place.

The definitions a referenced service depends on are now carried into the
generated runtime alongside it.
