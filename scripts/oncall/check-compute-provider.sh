#!/usr/bin/env bash
# Triage helper for the WorkerControllerInstanceComputeProviderNotEnabled alert.
set -euo pipefail

KEY="workercontroller.compute_providers.enabled"

if [[ $# -ne 3 ]]; then
  echo "Usage: $0 <cell-id> <namespace> <compute-provider>" >&2
  echo "Example: $0 <s-123> <namespace>.<account-id> aws-lambda" >&2
  exit 1
fi
CELL_ID=$1 NAMESPACE=$2 PROVIDER=$3


for tool in ct jq; do
  if ! command -v "$tool" >/dev/null; then
    echo "Error: '$tool' is not installed or not on PATH." >&2
    case "$tool" in
      jq) echo "Install it with: brew install jq" >&2 ;;
      ct) echo "Set up cloud-tools by following the internal-tools runbook." >&2 ;;
    esac
    exit 1
  fi
done


case "$CELL_ID" in
  s-gc*) CLOUD=GCP; RULES="gcp-cloud-run" ;;
  s-a*)  CLOUD=AWS; RULES="aws-lambda aws-agentcore" ;;
  *)     CLOUD=unknown; RULES="" ;;
esac

# The namespace's own override, if it has one.
ns_override=$(ct ocld prod dynamic-config namespace get --namespace "$NAMESPACE" \
  | jq -c ".[\"$KEY\"] // null")


# Merged config for the namespace. Can hold the cell default and a namespace override.
merged=$(ct ocld prod dynamic-config namespace get-merged --namespace "$NAMESPACE" \
  | jq -c ".[\"$KEY\"] // []")

# Cell default: the entry with no constraints.
cell_default=$(jq -c 'map(select(.constraints | not))[0].value // []' <<<"$merged")

# What the namespace ends up allowed: the override if there is one, otherwise the cell default.
ns_effective=$(jq -c '(map(select(.constraints)) + map(select(.constraints | not)))[0].value // []' <<<"$merged")

# Returns success if $PROVIDER is in the given JSON list, e.g. ["aws-lambda","aws-agentcore"].
has() { jq -e --arg p "$PROVIDER" 'index($p) != null' <<<"$1" >/dev/null 2>&1; }

printf '%-24s %s\n' \
  "WDV compute provider:" "$PROVIDER" \
  "Cloud (from cell):"    "$CLOUD ($CELL_ID)" \
  "Allowed for cloud:"    "${RULES:-?}" \
  "Cell default:"         "$cell_default" \
  "Namespace override:"   "$ns_override" \
  "Namespace effective:"  "$ns_effective"
echo

#Verdict
if [[ $CLOUD == unknown ]]; then
  echo "VERDICT: Unknown cloud for cell '$CELL_ID'. Ask in #team-eng-compute."
elif [[ " $RULES " != *" $PROVIDER "* ]]; then
  echo "VERDICT: WDV is wrong. '$PROVIDER' is not valid on $CLOUD."
elif ! has "$ns_effective"; then
  if [[ $ns_override != null ]]; then
    echo "VERDICT: Config is wrong. The namespace override does not include '$PROVIDER'."
  else
    echo "VERDICT: Config is wrong. The cell default does not include '$PROVIDER'."
  fi
else
  echo "VERDICT: '$PROVIDER' is allowed, so the alert shouldn't fire."
fi



