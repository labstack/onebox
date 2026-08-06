# Conformance corpus

These cases are the acceptance test for the contract's validation. They exist because a
review found that two implementations could satisfy the prose and still accept
different projects. Every case was verified against the loader before this
change was proposed, and each becomes a fixture in task 3.2.

`accept` means the normalised project validates. `reject` means it fails.

## Enforced by the schema

| # | Case | Expected |
|---|---|---|
| 1 | minimum project | accept |
| 2 | explicit workloads block | accept |
| 3 | one-char identifier | accept |
| 4 | app starting ob- | reject |
| 5 | underscore identifier | reject |
| 6 | unknown top-level field | reject |
| 7 | x- extension accepted | accept |
| 8 | port out of range | reject |
| 9 | zero replicas | reject |
| 10 | job requires data_effect | reject |
| 11 | job with data_effect | accept |
| 12 | job data_effect unknown | accept |
| 13 | application with data_effect | reject |
| 14 | application with run | reject |
| 15 | worker with schedule | reject |
| 16 | scheduled job | accept |
| 17 | daemon role | accept |
| 18 | daemon with route | accept |
| 19 | routes list | accept |
| 20 | bad protocol | reject |
| 21 | absolute compose ref | reject |
| 22 | relative compose ref | accept |
| 23 | absolute env_file | reject |
| 24 | relative env_file | accept |
| 25 | internal .. in path is lexically ok | accept |
| 26 | base_path absolute | accept |
| 27 | duration in days | accept |
| 28 | duration micro sign | accept |
| 29 | calver minimum version | accept |
| 30 | non-calver minimum version | reject |
| 31 | valid plan schema | accept |
| 32 | incomplete plan schema | reject |
| 33 | arbitrary plan schema | reject |
| 34 | hook with local | accept |
| 35 | command as bare string | accept |
| 36 | protection on a workload | accept |
| 37 | secret as scalar path | accept |
| 38 | secret as object | accept |
| 39 | verification http | accept |
| 40 | verification workload without probe | reject |
| 41 | verification url with exec | reject |
| 42 | verification url contains advisory | accept |
| 43 | status code 600 | reject |
| 44 | json equals null | accept |
| 45 | migration with contains | reject |
| 46 | protection is no longer a field | reject |
| 46a | a near-miss field name | reject |
| 47 | override of image refused | reject |
| 48 | proxy kind none without a route | accept |
| 48a | proxy kind none with a route | reject |
| 48b | unmanaged proxy keeps its routes | accept |
| 49 | migration_policy expand-only | accept |
| 50 | persistence external | accept |
| 51 | volume scalar | accept |
| 52 | service with volumes | accept |
| 53 | service scalar | accept |

## Enforced by the loader, not the schema

CUE cannot express these without leaving a disjunction unresolvable or failing
against the bare definition, so the loader enforces them and the specification
states them. They are fixtures all the same.

| # | Case | Expected |
|---|---|---|
| L1 | no environments | reject |
| L2 | empty workloads block | reject |
| L3 | no workload source at all | reject |
| L4 | shorthand and workloads together | reject |
| L5 | top-level build and image together | reject |
| L6 | two sources on a workload | reject |
| L7 | domain without port | reject |
| L8 | domain and routes together | reject |
| L9 | empty routes list | reject |
| L10 | empty applied_revisions | reject |

## Defaults that must materialise

A default on an optional CUE field never appears in output. These four are
checked explicitly because the first draft got every one of them wrong.

| Path | Value |
|---|---|
| `base_path` | `/var/lib/ob` |
| `proxy.network` | `ob-ingress` |
| `deployment.retain_releases` | `5` |
| `environments.<env>.policy.require_approval` | `true` |
