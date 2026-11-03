#!/usr/bin/env bash
# Downloads provider plugins into ./providers, unpacked, through terraform or
# tofu itself: it resolves the registry, checks the signature and the checksums.
#
#   scripts/fetch-provider.sh terraform-coop/foreman 0.7.0 hashicorp/random 3.4.3
#
# Prints the path of each binary, for provider.binary_path: under
# registry.terraform.io with terraform, registry.opentofu.org with tofu.
set -euo pipefail

if (($# == 0 || $# % 2 != 0)); then
  echo "usage: $0 <namespace/type> <version> [<namespace/type> <version>]..." >&2
  exit 2
fi

TERRAFORM=${TERRAFORM:-$(command -v terraform || command -v tofu || true)}
if [[ -z $TERRAFORM ]]; then
  echo "$0: needs tofu or terraform in PATH, or TERRAFORM=<path>" >&2
  exit 1
fi
root=$(cd "$(dirname "$0")/.." && pwd)
cache=$root/providers
mkdir -p "$cache"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

{
  echo 'terraform {'
  echo '  required_providers {'
  i=0
  while (($# > 0)); do
    echo "    p$i = { source = \"$1\", version = \"= $2\" }"
    sources+=("$1") versions+=("$2")
    shift 2
    i=$((i + 1))
  done
  echo '  }'
  echo '}'
} >"$work/main.tf"

TF_PLUGIN_CACHE_DIR=$cache "$TERRAFORM" -chdir="$work" init -backend=false -input=false >&2

for i in "${!sources[@]}"; do
  type=${sources[$i]#*/}
  ls "$cache"/registry.*/"${sources[$i]}"/"${versions[$i]}"/*/terraform-provider-"$type"_*
done
