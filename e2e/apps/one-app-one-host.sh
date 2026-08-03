#!/bin/bash
# One application, one host. Provision, deploy from bare, verify, destroy.
set -uo pipefail
export PATH="/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"
APP="$1"; PORT="$2"; PATHQ="$3"; WL="$4"
NAME="ob-e2e-$APP"
S=/private/tmp/claude-501/-Users-v-Projects-labstack-onebox/3014aad7-7c6c-4ce6-866d-cd1e81e3b035/scratchpad/vm
REPO=/Users/v/Projects/labstack/onebox

cleanup() { hcloud server delete "$NAME" >/dev/null 2>&1; }
trap cleanup EXIT

hcloud server create --name "$NAME" --type cpx22 --image ubuntu-24.04 --location fsn1 \
  --ssh-key "v@labstack.com" --label purpose=onebox-e2e --label ephemeral=true >/dev/null 2>&1 || { echo "  provision FAILED"; exit 1; }
IP=$(hcloud server ip "$NAME")
# Cloud providers recycle addresses. A stale host key from a destroyed server
# makes ob refuse the connection, which is correct of it and unhelpful here.
ssh-keygen -R "$IP" >/dev/null 2>&1

for i in $(seq 1 40); do
  ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5 -o BatchMode=yes "root@$IP" true 2>/dev/null && break
  sleep 5
done

sed "s/root@TARGET/root@$IP/" "$REPO/e2e/apps/$APP.yml" > "$S/$APP.host.yml"

start=$(date +%s)
out=$(cd "$REPO" && timeout 900 go run ./cmd/ob up -c "$S/$APP.host.yml" --bootstrap --wait 8m 2>&1)
rc=$?
elapsed=$(( $(date +%s) - start ))

if [ $rc -ne 0 ]; then
  echo "  DEPLOY FAILED after ${elapsed}s: $(echo "$out" | grep '^✗' | head -1)"
  exit 1
fi

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
