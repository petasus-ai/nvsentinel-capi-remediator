#!/usr/bin/env bash
# Verifies that every Go file starts with the license header in
# hack/boilerplate.go.txt. gofmt keeps a //go:build constraint above
# everything else, so a file may start with one, followed by a blank line.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
expected="$(cat "${root}/hack/boilerplate.go.txt")"
lines="$(wc -l < "${root}/hack/boilerplate.go.txt")"
failed=0

while IFS= read -r file; do
  skip=0
  if [[ "$(head -n 1 "${file}")" == "//go:build "* ]]; then
    skip=2
  fi
  actual="$(sed -n "$((skip + 1)),$((skip + lines))p" "${file}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "missing or wrong license header: ${file#"${root}"/}"
    failed=1
  fi
done < <(find "${root}" -name '*.go' -not -path '*/vendor/*' -not -path '*/.git/*')

exit "${failed}"
