## MODIFIED Requirements

### Requirement: The overlay onto a Compose-referenced workload is exact

Onebox SHALL add exactly the following to a Compose-referenced workload, and
SHALL modify nothing else:

| Addition | Exact content |
|---|---|
| Ingress network | append the project's `proxy.network`, resolved for the environment, to the service's networks, preserving existing entries in order |
| Identity labels | `ob.app`, `ob.release`, `ob.workload` |
| Routing labels | `traefik.enable`, `traefik.docker.network`, and per route: `traefik.<protocol>.routers.<router>.rule`, `.entrypoints`, `.tls`, and `traefik.<protocol>.services.<service>.loadbalancer.server.port`, using the router and proxy service names from the naming table |
| Environment projection | append the workload's resolved `env_files` entries, as staged paths, then its managed-service connection files, to the service's `env_file` list, preserving referenced entries first and in order; create the key when absent |

The ingress network SHALL be the project's `proxy.network` as resolved for the
selected environment, not a fixed name. When `proxy.kind` is `none` or `proxy.managed` is false, Onebox SHALL
add neither routing labels nor a network, and a workload declaring a route under
that configuration SHALL fail validation naming the conflict. Routing labels SHALL
otherwise be added only when the workload declares at least one route.

The overlay SHALL be refused, naming the key and the file, when the referenced
service already attaches the ingress network, already declares a label in the
`ob.` namespace, or — when the workload declares a route — already declares a
label in the `traefik.` namespace.

A referenced file SHALL be read as plain YAML rather than through a Compose
loader. Loading would interpolate variables, follow `extends` and `include`, and
validate the whole file, so referencing one service would fail on an unrelated
service's missing variable. Referencing one service SHALL depend only on that
service.

`container_name` SHALL be refused unconditionally, and SHALL NOT be silently
removed. Onebox owns container naming, because container names are host-global
and an authored name reintroduces exactly the cross-application collision the
naming contract exists to prevent. An earlier draft permitted a fixed name on a
single-replica recreate workload; that exception contradicted the naming
contract, since a preserved name such as `feed` is not application-scoped.
Conversion removes the key, which is a one-line edit.

A referenced service declaring `network_mode` SHALL be refused **when a network
would be attached**, because the container runtime rejects a service carrying
both `network_mode` and `networks`. When the proxy is disabled no network is
attached, so `network_mode` SHALL be preserved.

The projection row exists because a workload's environment follows from its
role, and a workload adopted from a Compose file has a role like any other.
Without it the overlay silently withheld the project's environment from every
`compose:`-sourced workload, whatever its role — behaviour nothing stated and
nobody chose. "SHALL modify nothing else" continues to hold: the enumeration is
larger, not weaker.

The overlay SHALL additionally be refused, naming the variable, the service and
the file, when the referenced service's `environment` sets a variable a
managed-service connection supplies to the workload. The container runtime
places `environment` above `env_file`, so such a key would outrank the
connection, and a credential generated on the target exists nowhere else.

Ejection SHALL strip everything the overlay adds, which now includes the
projected `env_file` entries, so the ejected file remains ordinary
user-authored Compose and generating from it again does not duplicate them.

#### Scenario: Projection appends and preserves
- **WHEN** a referenced service already declares an `env_file` list and the workload resolves two entries
- **THEN** the generated service's `env_file` carries the referenced entries first, then the resolved entries in order, then any connection files, and no other key changes

#### Scenario: A referenced environment key claiming a connection variable is refused
- **WHEN** a compose-sourced workload needs a managed service and the referenced service's `environment` sets a variable that connection supplies
- **THEN** generation fails naming the variable, the service and the file

#### Scenario: Ejection strips the projection
- **WHEN** a workload whose runtime carries projected `env_file` entries is ejected and then generated again
- **THEN** the ejected file carries only what the author's declaration implies, and generation does not duplicate the entries

#### Scenario: Fixed container name is refused
- **WHEN** a Compose-referenced workload sets `container_name`
- **THEN** generation fails naming the key and the file, and the key is not removed

#### Scenario: network_mode with the proxy enabled
- **WHEN** a referenced Compose service declares `network_mode` and a network would be attached
- **THEN** generation fails naming the key

#### Scenario: network_mode with the proxy disabled
- **WHEN** a referenced Compose service declares `network_mode` and the proxy is disabled
- **THEN** generation succeeds and the key is preserved

#### Scenario: Proxy disabled
- **WHEN** the environment disables the proxy and a workload declares a route
- **THEN** validation fails naming the conflict, and no routing label or network is added

#### Scenario: Existing routing labels conflict
- **WHEN** a Compose-referenced workload declares a route and the referenced service already sets a `traefik.` label
- **THEN** generation fails naming the label and the file

#### Scenario: Existing networks are preserved
- **WHEN** a Compose-referenced workload already attaches its own networks
- **THEN** the generated runtime retains them in order with the configured ingress network appended

#### Scenario: Keys outside the overlay are untouched
- **WHEN** a runtime is generated from a Compose-referenced workload
- **THEN** no key outside the overlay is added, removed, or modified
