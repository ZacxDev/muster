#!/usr/bin/env bash
# extract.sh — pull the chief backtest's cases from a muster database, READ ONLY.
#
#   KUBECONFIG=… cmd/muster-chief-backtest/extract.sh > cases.jsonl
#
# The session is read-only (default_transaction_read_only=on, set before the
# query) and scope 'all' (row-level security, as the operator reads it). The
# output holds production message text: keep it OUT of the repository (write it
# to a scratch directory), it is the harness's input, never a fixture.
#
# Environment (defaults are the workbench deployment's):
#   BT_NAMESPACE (muster)  BT_DEPLOY (deploy/muster-postgres)
#   BT_CHIEF (105)  BT_DECIDE (40)  BT_AUTO (7)  BT_OWNER_IDS (3008,3264,3533)
set -euo pipefail
here=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null && pwd)
kubectl -n "${BT_NAMESPACE:-muster}" exec -i "${BT_DEPLOY:-deploy/muster-postgres}" -- \
  psql -U muster -d muster -X -A -t -q -v ON_ERROR_STOP=1 \
  -v chief="${BT_CHIEF:-105}" -v decide="${BT_DECIDE:-40}" -v auto="${BT_AUTO:-7}" \
  -v owner_ids="${BT_OWNER_IDS:-3008,3264,3533}" \
  -c "set default_transaction_read_only = on" -c "set muster.scope = 'all'" -f - < "$here/extract.sql" |
  grep -v '^SET$' | grep -v '^$'
