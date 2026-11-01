#!/usr/bin/env bash
# Samples memory of a node running Kata workspaces, one line per sample, so
# you can watch how /dev/shm, QEMU RSS and guest memory develop over time.
#
#   NODE=kube-w4 [INTERVAL=30] [COUNT=0] [GUEST=1] ./scripts/kata-memstats.sh
#
# Node side (over ssh): used/available MB, /dev/shm used/size, QEMU process
# count, summed and per-VM RSS. Guest side (kubectl exec into the podman
# sidecar of every Kata pod scheduled on the node): MemFree/Cached per pod.
set -euo pipefail

NODE="${NODE:?set NODE, e.g. NODE=kube-w4}"
SSH_USER="${SSH_USER:-root}"
INTERVAL="${INTERVAL:-30}"
COUNT="${COUNT:-0}"
GUEST="${GUEST:-1}"
GUEST_CONTAINER="${GUEST_CONTAINER:-k8shell-podman}"

node_stats() {
  ssh -o BatchMode=yes "${SSH_USER}@${NODE}" '
    mem=$(free -m | awk "/^Mem:/ {printf \"used=%dMB avail=%dMB\", \$3, \$7}")
    shm=$(df -m /dev/shm | awk "NR==2 {printf \"shm=%d/%dMB(%s)\", \$3, \$2, \$5}")
    rss=$(ps -eo rss,args | awk "/[q]emu-system/ {n++; s+=\$1; l=l sprintf(\" %d\", \$1/1024)} END {printf \"qemu=%d rss=%dMB [%s ]\", n, s/1024, l}")
    echo "$mem | $shm | $rss"'
}

# Prints "ns/pod" for every pod with a runtimeClassName on $NODE.
kata_pods() {
  kubectl get pods -A --field-selector "spec.nodeName=${NODE}" \
    -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,RC:.spec.runtimeClassName' --no-headers |
    awk '$3 != "<none>" {print $1 "/" $2}'
}

guest_stats() {
  local out="" ref ns pod line
  for ref in $(kata_pods); do
    ns="${ref%%/*}"
    pod="${ref##*/}"
    line=$(kubectl -n "$ns" exec "$pod" -c "$GUEST_CONTAINER" -- \
      sh -c 'awk "/^MemFree/ {f=\$2} /^Cached/ {c=\$2} END {printf \"free=%dMB cached=%dMB\", f/1024, c/1024}" /proc/meminfo' 2>/dev/null) || line="unreachable"
    out+=" ${pod}(${line})"
  done
  printf '%s' "$out"
}

i=0
while :; do
  ts=$(date +%H:%M:%S)
  line=$(node_stats) || line="node unreachable"
  if [[ "$GUEST" == "1" ]]; then
    line+=" | guests:$(guest_stats || true)"
  fi
  echo "${ts} ${line}"
  i=$((i + 1))
  if [[ "$COUNT" != "0" && "$i" -ge "$COUNT" ]]; then
    break
  fi
  sleep "$INTERVAL"
done
