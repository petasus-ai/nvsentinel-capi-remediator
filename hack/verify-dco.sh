#!/usr/bin/env bash
# Verifies that every commit in BASE..HEAD carries a Developer Certificate of
# Origin sign-off by its author: a Signed-off-by trailer with the author's
# email. Merge commits are skipped, and so are commits authored by GitHub
# Apps such as Dependabot, whose sign-off names the app's support address.
#
# usage: hack/verify-dco.sh BASE [HEAD]
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 BASE [HEAD]" >&2
  exit 2
fi
base="$1"
head="${2:-HEAD}"
failed=0

while IFS= read -r commit; do
  email="$(git log -1 --format='%ae' "${commit}")"
  if [[ "${email}" == *'[bot]@users.noreply.github.com' ]]; then
    continue
  fi
  signoffs="$(git log -1 --format='%(trailers:key=Signed-off-by,valueonly)' "${commit}")"
  if ! grep -qiF "<${email}>" <<<"${signoffs}"; then
    echo "not signed off by its author <${email}>: $(git log -1 --format='%h %s' "${commit}")"
    failed=1
  fi
done < <(git rev-list --no-merges "${base}..${head}")

if [[ "${failed}" -ne 0 ]]; then
  echo
  echo "Sign off with 'git commit --amend -s' for the last commit, or"
  echo "'git rebase --signoff ${base}' for all of them, then push again."
fi
exit "${failed}"
