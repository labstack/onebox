# Onebox MCP quick start

Onebox now exposes its first agent-facing interface over MCP stdio. This
milestone is deliberately read-only: an MCP client can inspect a configured
single-host application, prepare a deployment proposal, read resolved
operational memory, and propose a typed memory change. It cannot execute a
deployment, rewrite configuration, or otherwise mutate production.

The project must use the stable `api_version: onebox.run/v1` contract. See the
[schema guide](schema-v1.md) for a complete example and the one-time migration
from the earlier alpha shape.

## Build

Build or install `ob` into the user-local directory used by the project
Justfile, and ensure that directory is on `PATH`:

```sh
just build       # or: just install
ob version --json
ob doctor --json
```

Both targets write `~/.local/bin/ob`; add it to `PATH` if needed, set
`OB_BIN_DIR` to change the directory, or set `OB_VERSION` to inject a release
version. `ob version` reports the running
release, VCS/build provenance, Go version, and supported executable-plan
schemas. `ob doctor` also checks PATH shadowing/stale candidates, SSH-agent
usability, project policy compatibility, approval support, and unavailable
declared protection capabilities.

The MCP client launches `ob mcp`; you normally do not run that command in a
terminal yourself.

## Configure an MCP client

For clients that accept an `mcpServers` JSON object, use absolute paths for
both the binary and project configuration:

```json
{
  "mcpServers": {
    "onebox": {
      "command": "/Users/you/.local/bin/ob",
      "args": [
        "--config",
        "/Users/you/src/my-app/ob.yml",
        "--env",
        "production",
        "mcp"
      ]
    }
  }
}
```

Put that entry in the configuration location required by your MCP client, then
restart the client. Absolute paths matter because clients do not necessarily
launch integrations from your project directory.

`ob mcp` reserves stdout exclusively for MCP protocol messages. Diagnostics go
to stderr. Do not wrap the command with a script that prints banners, status
lines, or other text to stdout.

The selected environment can control agent access explicitly:

```yaml
api_version: onebox.run/v1
environments:
  production:
    target: deploy@app.example.com
    policy:
      require_approval: true
      allow_agent_proposals: true
components:
  web:
    type: application
    service: web
    deployment:
      strategy: recreate
deployment:
  order: [web]
```

Both policy values default to `true`. Set `allow_agent_proposals: false` when
an agent may observe an environment but must not prepare a deployment proposal
for it. `require_approval` is included in typed results and enforced by local
CLI deployment through an exact plan-bound grant; the current MCP has no
execution or approval-minting tool regardless of its value. Environments may
also set `minimum_onebox_version` and `minimum_plan_schema` so an older local
runner fails closed during planning and execution.

These values do not sandbox a shell-capable agent from the local CLI. Local
approval receipts prove a CLI ceremony but are not external identity-provider
signatures. Only grant MCP access—not general shell access—when policy must be
a boundary the agent cannot bypass.

## Available tools

- `onebox_observe` returns a timestamped, typed, redaction-safe snapshot of the
  configured application, typed components, environment policy, declared
  observability, current release, workload health, supporting services,
  incomplete operations, drift, provenance, and observation warnings.
- `onebox_propose_deploy` reads configuration and target state, resolves image
  digests where possible, and returns a state-bound deployment proposal with
  keyed content commitments, readiness blockers, the canonical typed operation
  graph, an opaque structural Compose diff, command summary, risks, and
  verification outline. Every scalar Compose value
  uses a per-proposal keyed marker, so values cannot be read, brute-forced from
  a stable hash, or correlated across proposals. Image results expose the
  service and resolved immutable digest only; mutable source references and
  interpolated tags remain hidden.
- `onebox_read_memory` returns a deterministic, revision-bound, redaction-safe
  projection of resolved component semantics, deployment and migration policy,
  and declared protection and observability. It reads local project files and
  does not contact or mutate production.
- `onebox_propose_memory_change` accepts an expected revision, rationale, and a
  narrow typed patch. It returns an immutable, expiring, digest-bound proposal;
  it never edits `ob.yml` or changes policy. Stale revisions, unknown
  components, secret-like input, and untyped changes are rejected.

Each MCP process is bound to the `--env` value supplied when it is launched;
tool input cannot switch environments. Configure a separate MCP entry when you
want an agent to access another environment. All tools are marked read-only. A
proposal describes work; it does not authorize, apply, or perform it.

Component protection and observability fields are desired state. MCP results
report whether a declared capability is actually managed; the current local
slice reports backup/restore-drill and continuous observability capabilities as
unmanaged. A declaration alone is never presented as proof that protection or
monitoring is running.

Operator-authored lifecycle hook and job-command bodies are hidden. Lifecycle
hooks block proposal readiness because their effects are untyped; job readiness
uses the component's required `data_effect` declaration and migration policy,
which remain operator assertions. Top-level hooks are limited to `bootstrap`,
`pre_release`, `post_release`, and `post_deploy`; job commands live under
`components.<job>.command`. SOPS values are never decrypted by this read-only
MCP tool; the encrypted source is hash-bound and the proposal reports that
runtime payload materialization still requires the local execution path.

## CLI execution handoff

MCP proposals remain read-only descriptions; they are not executable approval
artifacts. Create the local state-bound plan and grant explicitly:

```sh
ob plan --out ob-plan.json
ob approve --plan ob-plan.json --out ob-approval.json
ob deploy --plan ob-plan.json --approval ob-approval.json
```

The current executable envelope is
`onebox.run/executable-deploy-plan/v1alpha2`. Missing or legacy schemas are
rejected before target connection with guidance to update the PATH-selected
binary and re-plan. The grant is mode-`0600`, digest-protected, expires no later
than the plan, and binds the exact application, environment, target, risk,
config, Compose, observed/live state, and payload.

When environment policy requires migration backup evidence, an external backup
process produces a secret-free `onebox.run/migration-backup-facts/v1alpha1`
manifest. Seal and apply it with:

```sh
ob backup-evidence create --plan ob-plan.json \
  --manifest backup-facts.json --out ob-backup-evidence.json
ob deploy --plan ob-plan.json --approval ob-approval.json \
  --backup-evidence ob-backup-evidence.json
```

Onebox validates the facts and binding; it does not perform the backup. The
audited `--override-migration-backup "reason"` path is mutually exclusive with
the receipt and requires strong plan-bound approval.

Migration containers write `onebox.run/job-result/v1alpha1` evidence to
`$OB_RESULT_FILE`. Provider-aware Atlas results carry unique ordered before/after
revisions; missing or invalid migration results become `changed=unknown`, halt
workload replacement without strong approval, and disable automatic rollback.
Verification can bind expected status codes, exact headers, scalar JSON paths,
and the complete expected migration revision list. See the
[schema guide](schema-v1.md) for the exact fields.

For automation, `--output json` returns versioned plan/status/operation
envelopes and `--output ndjson` streams redaction-safe operation events followed
by a result or error record. Structured deploy requires `--plan`; stdout stays
machine-readable while local diagnostics remain on stderr.

## SSH and target prerequisites

The selected environment must point to a reachable host. Onebox uses the same
secure SSH transport as the CLI:

- Load a usable identity into the SSH agent inherited by the MCP client, or
  provide an unencrypted `~/.ssh/id_ed25519` or `~/.ssh/id_rsa`.
- Record the target host key in `~/.ssh/known_hosts`, for example by connecting
  once with your normal `ssh` client. Onebox never disables host-key checking.
- Ensure the configured SSH user can run Docker on the target. Deployment
  proposals also use target-side `docker buildx imagetools inspect` to resolve
  registry digests, so the target needs any required registry credentials.

The MCP client must inherit the relevant environment, especially
`SSH_AUTH_SOCK` when agent-based authentication is used.

## Current safety boundary

There is intentionally no MCP deploy-execution tool yet. Continue to use the
reviewed CLI plan/deploy workflow for mutations. CLI planning and execution now
share the canonical operation graph, exact plan/grant bindings, runner policy,
migration backup and job-result gates, drift checks, fencing, structured events,
and engine audit mechanisms. A later milestone can expose execution only
through equally bound, authenticated approval authority.

The CLI is therefore useful now, not a competing product direction: it is the
deterministic mutation path, the development and CI surface, and the
break-glass adapter if an MCP client is unavailable. MCP becomes the normal user
interface because it gives an agent typed, secret-safe perception and proposals;
it should not become a thin alias for arbitrary CLI or SSH commands. Both
adapters must continue sharing the same operations service and engine safety
rules.
