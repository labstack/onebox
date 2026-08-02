# Conformance corpus

These cases are the acceptance test for `schema.cue`. They exist because a
review found that two implementations could satisfy the prose and still accept
different projects. Every case below was verified against `schema.cue` before
this change was proposed; they become the fixture corpus for task 3.8.

`accept` means the project validates. `reject` means validation fails.

| # | Case | Expected |
|---|---|---|
| 1 | Minimum project: identifier, server, one build source | accept |
| 2 | One-character identifier (`app: a`) | accept |
| 3 | Application identifier beginning `ob-` | reject |
| 4 | Identifier containing an underscore | reject |
| 5 | Unknown top-level field | reject |
| 6 | `x-` extension key | accept |
| 7 | Port outside 1–65535 | reject |
| 8 | `replicas: 0` | reject |
| 9 | Workload declaring both `build` and `image` | reject |
| 10 | Job with `data_effect: unknown` | accept |
| 11 | `migration_policy: expand-only` | accept |
| 12 | `persistence.mode: external` | accept |
| 13 | Volume declared as a scalar identifier | accept |
| 14 | Service declaring its volume identifiers | accept |
| 15 | Service declared as a scalar version | accept |
| 16 | Route with `protocol: tcp`, an entrypoint, and `tls: passthrough` | accept |
| 17 | Route with an unsupported protocol | reject |
| 18 | Absolute path in `runtime.env_files` | reject |
| 19 | `runtime.env_files` entry escaping the repository (`../`) | reject |
| 20 | Repository-relative `runtime.env_files` entry | accept |
| 21 | Absolute `base_path` | accept |
| 22 | Hook declared with `local: true` | accept |
| 23 | Job `command` in the hook form | accept |
| 24 | `protection` declared on a workload | accept |
| 25 | `proxy: {kind: none, managed: false}` | accept |
| 26 | Verification with `contains` and `advisory` | accept |
| 27 | Environment override setting a mapping key to null | accept |
| 28 | Environment override of a workload's `image` | reject |

Cases 3, 4, 18, 19, and 28 are the ones a permissive implementation is most
likely to get wrong, and each of them protects something that cannot be fixed
later: the generated-name namespace, name injectivity, path containment, and
the override boundary.
