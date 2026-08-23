#!/usr/bin/env bash
#
# Boots the throwaway server the `server-e2e` suite deploys to, and derives the
# connection details from the running instance.
#
# The suite itself runs on this machine, not in the guest. ob's user position
# is a workstation with SSH to a rented box — internal/transport/transport.go
# records that the docker suite substitutes local docker for that server — and
# this is the one harness that does not make the substitution. Running the
# tests inside the guest would relocate the blind spot rather than close it.
set -euo pipefail

instance="onebox-e2e"
config="e2e/lima.yaml"
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Lima's own ssh.config is the supported source for an instance's connection
# details. `limactl show-ssh` reads the same thing and is deprecated as of
# Lima 2.2, so it is not used here.
instance_ssh_config() {
	local path="${LIMA_HOME:-$HOME/.lima}/${instance}/ssh.config"
	[[ -r "$path" ]] || {
		echo "no ssh.config for instance ${instance}; run 'just lima-up' first" >&2
		return 1
	}
	printf '%s\n' "$path"
}

ssh_field() {
	local field="$1" path
	path="$(instance_ssh_config)"
	awk -v field="$field" '
		$1 == field { gsub(/"/, "", $2); print $2; exit }
	' "$path"
}

up() {
	local status
	if limactl list --quiet 2>/dev/null | grep -qx "$instance"; then
		status="$(limactl list --format '{{.Status}}' "$instance" 2>/dev/null | tr '[:upper:]' '[:lower:]')"
		if [[ "$status" != "running" ]]; then
			limactl start --tty=false "$instance"
		else
			echo "instance ${instance} already exists; reusing it"
		fi
	else
		limactl start --name="$instance" --tty=false "${repo}/${config}"
	fi

	local port key
	port="$(ssh_field Port)"
	key="$(ssh_field IdentityFile)"

	# ob connects as root. Proving that here, at boot, keeps a permissions
	# problem from surfacing later as an unrelated-looking deploy failure.
	if ! ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 \
		-i "$key" -p "$port" root@127.0.0.1 true; then
		echo "the guest is up but refuses root over SSH; ob does not elevate, so the suite cannot run" >&2
		return 1
	fi
	echo "server ready: root@127.0.0.1:${port}"
}

# Prints the environment the suite reads. Kept separate from `test` so it can
# be eval'd into a shell for running individual cases by hand.
env_lines() {
	local port key
	port="$(ssh_field Port)"
	key="$(ssh_field IdentityFile)"
	# The same string shape ob.yml's `server:` field takes, parsed by the same
	# code: internal/target.Address is [user@]host[:port].
	printf 'ONEBOX_SERVER_E2E=1\n'
	printf 'ONEBOX_E2E_SERVER=root@127.0.0.1:%s\n' "$port"
	printf 'ONEBOX_E2E_SERVER_KEY=%s\n' "$key"
}

run_tests() {
	local -a env=()
	while IFS= read -r line; do env+=("$line"); done < <(env_lines)
	# -count=1 because a cached pass against a guest that has since changed is
	# a green tick for work nobody did.
	env ONEBOX_E2E=1 "${env[@]}" \
		go test "${repo}/e2e/" -count=1 -timeout 40m -run Server "$@"
}

case "${1:-}" in
up) up ;;
env) env_lines ;;
test)
	shift
	run_tests "$@"
	;;
down) limactl delete -f "$instance" ;;
*)
	echo "usage: $0 {up|env|test|down}" >&2
	exit 2
	;;
esac
