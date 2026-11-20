#!/usr/bin/env bash

set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

private_go_modules="${GOPRIVATE:-github.com/InjectiveLabs}"
private_no_sum_db="${GONOSUMDB:-github.com/InjectiveLabs}"

export GOPRIVATE="${private_go_modules}"
export GONOSUMDB="${private_no_sum_db}"

if [[ -z "${GH_TOKEN:-}" ]]; then
  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    echo "::error::Missing GH_TOKEN secret. It must have read access to private InjectiveLabs Go modules."
    exit 1
  fi

  exec "$@"
fi

# Keep the credential command-scoped. In particular, do not leave it in the
# repository or global Git config where a later build/review step could read it.
auth_header="$(printf 'x-access-token:%s' "${GH_TOKEN}" | base64 | tr -d '\r\n')"
if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  echo "::add-mask::${GH_TOKEN}"
  echo "::add-mask::${auth_header}"
fi
unset GH_TOKEN

export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0="http.https://github.com/InjectiveLabs/.extraheader"
export GIT_CONFIG_VALUE_0="AUTHORIZATION: basic ${auth_header}"
unset auth_header

exec "$@"
