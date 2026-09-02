#!/usr/bin/env bash
# Verifies that every Go file starts with the license header in
# hack/boilerplate.go.txt.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
expected="$(cat "${root}/hack/boilerplate.go.txt")"
lines="$(wc -l < "${root}/hack/boilerplate.go.txt")"
failed=0

while IFS= read -r file; do
  actual="$(head -n "${lines}" "${file}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "missing or wrong license header: ${file#"${root}"/}"
    failed=1
  fi
done < <(find "${root}" -name '*.go' -not -path '*/vendor/*' -not -path '*/.git/*')

exit "${failed}"
