# The `ob` command line

> Status: the interface the current binary presents.
>
> See the [documentation authority map](README.md) for how this relates to the
> capability contracts.

The CLI is the interface, for people and for agents alike. Point an agent at
`ob` the way you would point it at `git` or `gh`: every command states whether
it contacts the target and whether it changes anything, `--output json` gives
versioned structured results for the operational ones, and diagnostics stay on
stderr so the structured stream is never polluted.

`ob <command> --help` is authoritative and longer than this page. This page is
the map.

## Complete command surface

An operator can afford to find out by trying. An agent cannot. So every command
answers target contact, mutation, and machine-output support up front. Group
commands only provide help and dispatch to the indented operation.

| Command | Contacts target | Changes target | Machine output / local effect |
|---|---|---|---|
| `ob abort` | yes | **yes** | JSON/NDJSON; reverts an interrupted deploy |
| `ob approve` | no | no | writes a grant locally; interactive human output |
| `ob audit` | reads | no | human table |
| `ob backup-evidence` | no | no | command group |
| `ob backup-evidence create` | no | no | JSON/NDJSON; writes a receipt locally |
| `ob bootstrap` | yes | **yes** | JSON/NDJSON; prepares the host |
| `ob canonical` | no | no | JSON/NDJSON or canonical YAML |
| `ob deploy` | yes | **yes** | JSON/NDJSON; releases |
| `ob destroy` | yes | **yes** | interactive human output; volumes kept unless requested |
| `ob doctor` | local reads | no | `--json`, JSON/NDJSON, or human report |
| `ob eject` | no | no | JSON/NDJSON; rewrites project files locally |
| `ob exec` | yes | **whatever the command does** | human stream; outside the journal |
| `ob init` | no | no | writes `ob.yml` locally |
| `ob logs` | reads | no | human log stream |
| `ob plan` | reads | no | JSON/NDJSON; writes a plan locally |
| `ob preflight` | reads | no | human report |
| `ob preview` | no | no | JSON/NDJSON or rendered YAML |
| `ob proxy` | no | no | command group |
| `ob proxy apply` | yes | **yes** | JSON/NDJSON; converges the shared proxy |
| `ob resume` | yes | **yes** | JSON/NDJSON; finishes an interrupted deploy |
| `ob rollback` | yes | **yes** | JSON/NDJSON; activates the previous release |
| `ob schema` | no | no | writes JSON Schema to stdout or `--to` |
| `ob secrets` | no | no | command group |
| `ob secrets edit` | no | no | decrypts temporarily and re-encrypts locally |
| `ob secrets push` | yes | **yes** | human output; updates the current release |
| `ob service` | no | no | command group |
| `ob service apply` | yes | **yes** | JSON/NDJSON; converges supporting services |
| `ob status` | reads | no | JSON/NDJSON or human report; non-zero on divergence |
| `ob validate` | no | no | JSON/NDJSON or human summary |
| `ob version` | no | no | `--json` or human report |

`ob exec` is the one row without a definite answer, and that is the point: it runs
outside the journal and the safety regime, and nothing it changes belongs to any
release.

## The normal path

```sh
ob init                     # scaffold ob.yml from the Compose file you have
ob validate                 # the contract, offline
ob canonical                # what Onebox understood, and where each value came from
ob preview                  # the Compose runtime it will generate

ob bootstrap                # once per host: layout, registries, proxy, services
ob preflight                # ask the host whether this could be deployed

ob plan --out ob-plan.json                        # bind config, runtime, host state, images
ob approve --plan ob-plan.json --out ob-grant.json # a human act; there is no flag to skip it
ob deploy --plan ob-plan.json --approval ob-grant.json
```

`ob deploy` without `--plan` plans inline and asks for confirmation. The
two-artifact form exists so that what was reviewed is exactly what is applied:
the plan binds the configuration, the rendered runtime, the host state and the
pinned images, and expires after fifteen minutes.

## When something goes wrong

```sh
ob status        # recorded versus actual; non-zero exit on divergence
ob audit         # who did what, when — one row per invocation
ob logs web      # from the current release
ob resume        # the runner died; finish what it started
ob abort         # the runner died; put the previous release back
ob rollback      # the release was bad; re-activate the previous one
```

`resume` and `abort` are for an interrupted *runner*. `rollback` is for a
completed release that turned out to be wrong. Both abort and rollback are gated
on what already happened to data: a migration that cannot be undone by moving a
symlink refuses, because reverting the containers would leave them running
against data they do not match.

## For agents

- **Structured output**: the command table is the support matrix. Passing
  `--output json` or `--output ndjson` to any other command is an error, never a
  silently ignored request.
- **Diagnostics never enter that stream.** They go to stderr.
- **No secrets in the structured stream.** It is always redacted, and `--raw`
  is refused alongside `--output json` rather than silently ignored.
- **Failures are typed.** Project-contract failures carry a stable code, field
  path, and where possible the authored line and corrective command. Operational
  envelopes carry a stable error code and safe message. Branch on the code, not
  the sentence.
- **Exit codes mean something.** `ob status` exits non-zero on divergence.
- **Application releases only through `deploy`.** There is deliberately no
  second way to release application workloads onto a box. Bootstrap, service,
  proxy, secrets, recovery, destroy, and the explicit exec escape hatch perform
  the other mutations named in the table.

```sh
ob status --output json | jq -r '.status.diverged'
ob validate --output json | jq -r '.schema_version'
```

### Structured output contracts

`json` emits one indented document. `ndjson` emits compact JSON records, one per
line. Read-only commands generally emit one record; operations stream event
records and end with a result or error record.

| Command(s) | `schema_version` | Top-level content |
|---|---|---|
| `ob validate` | `onebox.run/cli-validate/v1alpha1` | `app`, `environment`, `workloads`, `jobs`, `services`, `error` |
| `ob canonical` | `onebox.run/cli-canonical/v1alpha1` | `environment`, redacted `document`, `redacted`, `origins`, `error` |
| `ob preview` | `onebox.run/cli-preview/v1alpha1` | `environment`, `release`, `digest`, redacted `runtime`, `services`, `error` |
| `ob eject` | `onebox.run/cli-eject/v1alpha1` | written `runtime`, handed-over `workloads`, `error` |
| `ob status` | `onebox.run/status/v1alpha1` | `status`, `error` |
| `ob plan` | `onebox.run/executable-deploy-plan/v1alpha2` | executable plan, operation graph, artifacts, bindings, digest, expiry |
| mutation commands in the table (`json`) | `onebox.run/cli-operation/v1alpha1` | ordered `events`, terminal `result`, `error` |
| mutation commands in the table (`ndjson`) | `onebox.run/cli-record/v1alpha1` | `type` plus one `event`, `result`, or `error` |
| `ob backup-evidence create` | `onebox.run/migration-backup-evidence/v1alpha1` | plan-bound evidence receipt and digest |
| `ob doctor` | `onebox.run/doctor-report/v1alpha1` | overall `status`, `binary`, `ssh_agent`, `project`, `approval`, `protections` |
| `ob version --json` | `onebox.run/version-report/v1alpha1` | build provenance and supported executable plan schemas |

Consumers should select behavior from `schema_version` and ignore fields they do
not use. A consumer-visible incompatible shape receives a new schema version.
An `error` object contains stable `code` and safe `message` fields; project
validation errors also include `path` and, when available, a corrective `next`
command. Detailed diagnostics remain on stderr.

## Global flags

| Flag | Meaning |
|---|---|
| `-c, --config` | path to `ob.yml` (default `ob.yml`) |
| `-e, --env` | environment name (default `production`) |
| `--output` | `human`, `json`, or `ndjson`; only commands marked in the support matrix accept structured modes |
| `-v, --verbose` | print every remote command before it runs |

`--verbose` is the honest way to see what Onebox does to a host: it prints each
remote command. Nothing is hidden behind it.
