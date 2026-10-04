#!/usr/bin/env bash
# Exercise dpkg's unpacked-package lifecycle without configuring dependencies or
# touching the host package database. This is not GNOME installation acceptance.
set -euo pipefail
package="${1:?usage: check-desktop-deb-lifecycle.sh package.deb}"
for command in dpkg dpkg-deb dpkg-query cmp cp diff find grep mkdir mktemp realpath rm stat touch; do
  command -v "$command" >/dev/null || { echo "Missing prerequisite: $command" >&2; exit 1; }
done
[[ $EUID == 0 ]] || { echo 'Run as root to verify dpkg payload ownership in an isolated root.' >&2; exit 1; }
package="$(realpath "$package")"
name="$(dpkg-deb --field "$package" Package)"
version="$(dpkg-deb --field "$package" Version)"
[[ "$name" == gpu-workload-supervisor ]]
[[ "$(dpkg-deb --field "$package" Architecture)" == "$(dpkg --print-architecture)" ]]
workspace="$(mktemp -d)"
trap 'rm -rf "$workspace"' EXIT
root="$workspace/root"
expected="$workspace/expected"
control="$workspace/control"
mkdir -p "$root/var/lib/dpkg" "$expected"
touch "$root/var/lib/dpkg/status"
dpkg-deb --control "$package" "$control"
# A desktop package must never execute maintainer code against user sessions.
# Reject scripts before invoking dpkg, including ones that are not executable.
for script in preinst postinst prerm postrm triggers; do
  [[ ! -e "$control/$script" ]] || { echo "Unexpected control hook: $script" >&2; exit 1; }
done
dpkg-deb --extract "$package" "$expected"
[[ -n "$(find "$expected" -type f -print -quit)" ]]
[[ -z "$(find "$expected" ! -type d ! -type f -print -quit)" ]]
# Preserve representative operator data and integration outside package paths.
for path in \
  home/operator/.config/gpu-workload-supervisor/profile.json \
  home/operator/.local/state/gpu-workload-supervisor/state.db \
  home/operator/.local/state/gpu-workload-supervisor/audit.jsonl \
  home/operator/models/model.bin \
  home/operator/.config/systemd/user/custom-workload.service; do
  mkdir -p "$root/$(dirname "$path")"
  printf 'preserve %s\n' "$path" > "$root/$path"
done
cp -a "$root/home" "$workspace/preserved-home"
check_home() {
  diff -r "$workspace/preserved-home" "$root/home"
}
check_payload() {
  local path relative
  [[ "$(dpkg-query --admindir="$root/var/lib/dpkg" --show \
    --showformat='${Version} ${Status}' "$name")" == "$version install ok unpacked" ]]
  while IFS= read -r -d '' path; do
    relative="${path#"$expected"}"
    [[ -f "$root$relative" && ! -L "$root$relative" ]]
    cmp "$path" "$root$relative"
    [[ "$(stat -c '%a %u %g' "$path")" == "$(stat -c '%a %u %g' "$root$relative")" ]]
    dpkg-query --admindir="$root/var/lib/dpkg" --search "$relative" | \
      grep -Fx "$name: $relative" >/dev/null
  done < <(find "$expected" -type f -print0)
  check_home
}
check_removed() {
  local path relative
  while IFS= read -r -d '' path; do
    relative="${path#"$expected"}"
    [[ ! -e "$root$relative" && ! -L "$root$relative" ]]
  done < <(find "$expected" -type f -print0)
  check_home
}
# Use --root for every mutation: dpkg also sets its administrative directory
# beneath this private temporary tree. No dependency bypass flags are used.
dpkg --root="$root" --unpack "$package"
check_payload
dpkg --root="$root" --remove "$name"
check_removed
dpkg --root="$root" --purge "$name"
check_removed
[[ -z "$(dpkg-query --admindir="$root/var/lib/dpkg" --show \
  --showformat='${binary:Package}' 2>/dev/null)" ]]
dpkg --root="$root" --unpack "$package"
check_payload
echo "PASS: $name $version unpack/remove/purge/re-unpack; user data preserved."
