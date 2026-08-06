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

## The two questions to settle before running anything

An operator can afford to find out by trying. An agent cannot. So every command
answers both up front:

| Command | Contacts the target | Changes the target |
|---|---|---|
| `validate` | no | no |
| `canonical` | no | no |
| `preview` | no | no |
| `schema` | no | no |
| `init` | no | no — writes `ob.yml` in your repository |
| `eject` | no | no — writes the runtime into your repository, permanently |
| `version` | no | no |
| `doctor` | reads | no |
| `preflight` | reads | no |
| `status` | reads | no |
| `audit` | reads | no |
| `logs` | reads | no |
| `plan` | reads | no — writes a plan artifact locally |
| `approve` | no | no — writes a grant locally |
| `backup-evidence create` | no | no — writes a receipt locally |
| `bootstrap` | **yes** | **yes** — prepares the host |
| `deploy` | **yes** | **yes** — releases |
| `rollback` | **yes** | **yes** — re-activates the previous release |
| `resume` | **yes** | **yes** — finishes an interrupted deploy |
| `abort` | **yes** | **yes** — reverts an interrupted deploy |
| `destroy` | **yes** | **yes** — tears the application down |
| `exec` | **yes** | **whatever the command does** |
| `secrets edit` | no | no — decrypts to a temporary file, re-encrypts on save |
| `secrets push` | **yes** | **yes** — updates the current release |
| `service apply` | **yes** | **yes** — converges supporting services |
| `proxy apply` | **yes** | **yes** — reconfigures the shared proxy |

`exec` is the one row without a definite answer, and that is the point: it runs
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

- **Structured output**: `--output json` on `validate`, `canonical`, `preview`,
  `status`, `plan`, `deploy` and `eject`. Each emits one versioned envelope
  naming its own schema. `--output ndjson` streams deploy events as they happen.
- **Diagnostics never enter that stream.** They go to stderr.
- **No secrets in the structured stream.** It is always redacted, and `--raw`
  is refused alongside `--output json` rather than silently ignored.
- **Failures are typed.** Every refusal carries a stable code, the path that
  produced it, and where possible the line and the command that resolves it.
  Branch on the code, not the sentence.
- **Exit codes mean something.** `ob status` exits non-zero on divergence.
- **Nothing reaches a host except through `deploy`.** There is deliberately no
  second way to put an application on a box.

```sh
ob status --output json | jq -r '.status.diverged'
ob validate --output json | jq -r '.schema_version'
```

## Global flags

| Flag | Meaning |
|---|---|
| `-c, --config` | path to `ob.yml` (default `ob.yml`) |
| `-e, --env` | environment name (default `production`) |
| `--output` | `human`, `json`, or `ndjson` |
| `-v, --verbose` | print every remote command before it runs |

`--verbose` is the honest way to see what Onebox does to a host: it prints each
remote command. Nothing is hidden behind it.
