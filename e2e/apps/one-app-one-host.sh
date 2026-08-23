#!/bin/bash
# One application, one host. Provision, deploy from bare, verify, destroy.
set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
APP="$1"; PORT="$2"; PATHQ="$3"; WL="${4:-}"
NAME="ob-e2e-$APP"
S=${ONEBOX_E2E_SCRATCH:-/tmp}
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# The provisioning account is the operator's, not the repository's. The SSH key
# is whatever name `hcloud ssh-key list` shows for the key that can reach the
# host; the rest have defaults that are only a starting point.
ONEBOX_E2E_SSH_KEY="${ONEBOX_E2E_SSH_KEY:-}"
if [ -z "$ONEBOX_E2E_SSH_KEY" ]; then
  echo "ONEBOX_E2E_SSH_KEY must name an SSH key registered with hcloud (see: hcloud ssh-key list)" >&2
  exit 2
fi
ONEBOX_E2E_SERVER_TYPE="${ONEBOX_E2E_SERVER_TYPE:-cpx22}"
ONEBOX_E2E_IMAGE="${ONEBOX_E2E_IMAGE:-ubuntu-24.04}"
ONEBOX_E2E_LOCATION="${ONEBOX_E2E_LOCATION:-fsn1}"

cleanup() { hcloud server delete "$NAME" >/dev/null 2>&1; }
trap cleanup EXIT

hcloud server create --name "$NAME" --type "$ONEBOX_E2E_SERVER_TYPE" --image "$ONEBOX_E2E_IMAGE" --location "$ONEBOX_E2E_LOCATION" \
  --ssh-key "$ONEBOX_E2E_SSH_KEY" --label purpose=onebox-e2e --label ephemeral=true >/dev/null 2>&1 || { echo "  provision FAILED"; exit 1; }
IP=$(hcloud server ip "$NAME")
# Cloud providers recycle addresses. A stale host key from a destroyed server
# makes ob refuse the connection, which is correct of it and unhelpful here.
ssh-keygen -R "$IP" >/dev/null 2>&1

for i in $(seq 1 40); do
  ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o BatchMode=yes "root@$IP" true 2>/dev/null && break
  sleep 5
done

sed "s/root@TARGET/root@$IP/" "$REPO/e2e/apps/$APP.yml" > "$S/$APP.host.yml"

# Bootstrap then deploy, which is the only path: it holds the deploy lock,
# fences the runner, journals every phase, drains on handover, and can roll
# back. There is deliberately no second way to put an application on a host.
start=$(date +%s)
# Built once. `go run` recompiles per invocation and, worse, `timeout` kills
# the go wrapper while the real process it started keeps running — a hang then
# looks like a slow build instead of a stuck command.
OB="$S/ob"
(cd "$REPO" && go build -o "$OB" ./cmd/ob) || { echo "  BUILD FAILED"; exit 1; }

# Confirmation defaults on, so this takes the ceremony the way an operator would:
# plan, bind a local confirmation to that exact plan, apply both. Deploying is
# the only path onto a host, and the confirmed path is the only way through it.
#
# The prompt is answered rather than bypassed, because there is no flag to skip
# it and there should not be. A routine plan asks y/n; one that touches data
# asks for the release identifier to be typed back, so read the class from the
# plan and answer what it actually asked.
approval_answer() {
  python3 - "$1" <<'PYEOF'
import json, sys
plan = json.load(open(sys.argv[1]))
op = plan.get("operation", {})
print(op.get("release_id", "") if op.get("approval") in ("strong", "break_glass") else "yes")
PYEOF
}

out=$(cd "$REPO" && timeout 900 "$OB" bootstrap -c "$S/$APP.host.yml" 2>&1 \
  && timeout 900 "$OB" plan -c "$S/$APP.host.yml" --out "$S/$APP.plan.json" 2>&1 \
  && printf '%s\n' "$(approval_answer "$S/$APP.plan.json")" \
     | timeout 300 "$OB" approve -c "$S/$APP.host.yml" --plan "$S/$APP.plan.json" --out "$S/$APP.confirmation.json" 2>&1 \
  && timeout 900 "$OB" deploy -c "$S/$APP.host.yml" --plan "$S/$APP.plan.json" --approval "$S/$APP.confirmation.json" -y 2>&1)
rc=$?
elapsed=$(( $(date +%s) - start ))

if [ $rc -ne 0 ]; then
  echo "  DEPLOY FAILED after ${elapsed}s: $(echo "$out" | grep '^✗' | head -1)"
  exit 1
fi

if [ -z "$WL" ]; then
  WL=$("$OB" canonical -c "$S/$APP.host.yml" --output json 2>/dev/null | python3 -c '
import json,sys
doc = json.load(sys.stdin)["data"]["document"]
name, routed = None, None
for line in doc.splitlines():
    if line.startswith("  ") and line.endswith(":") and not line.startswith("    "):
        name = line.strip().rstrip(":")
    if name and ("domain:" in line or "routes:" in line) and routed is None:
        routed = name
print(routed or "")
')
fi
[ -n "$WL" ] || { echo "  could not determine the routed workload"; exit 1; }

# Verify from the host, not inside the container: depending on whichever of
# curl or wget an image happens to ship is not a property of the deploy.
ip=$(ssh -o BatchMode=yes "root@$IP" "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' \$(docker ps -q --filter label=ob.app=$APP --filter label=ob.workload=$WL | head -1) 2>/dev/null | awk '{print \$1}'" 2>/dev/null | tr -d '\r')
code=""
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  code=$(ssh -o BatchMode=yes "root@$IP" "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://$ip:$PORT$PATHQ" 2>/dev/null | tr -d '\r')
  case "$code" in 2*|3*) break;; esac
  sleep 6
done

running=$(ssh -o BatchMode=yes "root@$IP" "docker ps -q --filter label=ob.app=$APP | wc -l" 2>/dev/null | tr -d ' ')
vols=$(ssh -o BatchMode=yes "root@$IP" "docker volume ls --format '{{.Name}}' | grep -c '^ob_' || true" 2>/dev/null | tr -d ' ')
cur=$(ssh -o BatchMode=yes "root@$IP" "readlink /var/lib/ob/$APP/current" 2>/dev/null)

healthy=$(echo "$out" | grep '^healthy' | sed 's/^healthy *//')
echo "  ${elapsed}s  http=$code  containers=$running  volumes=$vols  healthy=[${healthy:-none declared}]"
echo "  release=$(basename ${cur:-none})"
[ "$code" = "200" ] || [ "$code" = "302" ] || { echo "  VERIFY FAILED (http=$code)"; exit 1; }
