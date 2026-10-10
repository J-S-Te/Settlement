#!/bin/sh
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mirror="$repo/third_party/license-core"
manifest="$repo/scripts/license-core.sha256"
files='clock.go diff.go go.mod license.go recovery.go runtime/lock_unix.go runtime/snapshot.go runtime/state.go syncclient/client.go consumer/consumer.go'
digest() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d ' ' -f 1; else shasum -a 256 "$1" | cut -d ' ' -f 1; fi; }
verify() {
 [ -z "$(find "$mirror" -type l -print)" ] || exit 1
 expected=$(printf '%s\n' $files | sort)
 actual=$(cd "$mirror" && find . -type f | sed 's|^./||' | sort)
 [ "$actual" = "$expected" ] || { echo 'commercial core allowlist mismatch' >&2; exit 1; }
 [ "$(awk '{print $2}' "$manifest" | sort)" = "$expected" ] || exit 1
 while read -r hash file; do [ "$(digest "$mirror/$file")" = "$hash" ] || { echo 'commercial core digest mismatch' >&2; exit 1; }; done < "$manifest"
}
source=${2:-$repo/../license-core}
verify_source() {
 [ -z "$(find "$source" -type l -print)" ] || { echo 'commercial core authority symlink rejected' >&2; exit 1; }
 expected=$(printf '%s\n' $files | sed '/^go.mod$/d' | sort)
 actual=$(cd "$source" && find . -type f -name '*.go' ! -name '*_test.go' ! -path './coverage/*' | sed 's|^./||' | sort)
 [ "$actual" = "$expected" ] || { echo 'review recursive commercial core allowlist before release' >&2; exit 1; }
}
case ${1:---check} in
 --sync)
  verify_source
  mkdir -p "$mirror"
  for file in $files; do mkdir -p "$mirror/$(dirname "$file")"; cp "$source/$file" "$mirror/$file"; done
  : > "$manifest"
  for file in $files; do printf '%s  %s\n' "$(digest "$mirror/$file")" "$file" >> "$manifest"; done
  verify ;;
 --check)
  verify
  if [ -d "$source" ]; then
   verify_source
   for file in $files; do
    cmp -s "$source/$file" "$mirror/$file" || { echo "commercial core authority drift: $file" >&2; exit 1; }
   done
  fi ;;
 *) exit 2 ;;
esac
