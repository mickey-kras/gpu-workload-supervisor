#!/usr/bin/env bash
# Destructive only to newly created test resources; run on a disposable systemd host.
set -euo pipefail
if [[ ${1:-} != --isolated-test-host || $# != 2 || $EUID != 0 ]]; then
  echo 'usage: sudo bash scripts/setup-desktop-integration.sh --isolated-test-host PACKAGE.deb' >&2
  exit 2
fi
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted ]]; then
  echo 'requires an ephemeral GitHub-hosted runner' >&2
  exit 2
fi
package=$(realpath "$2")
repo=$(cd "$(dirname "$0")/.." && pwd)
unit=gpu-workload-supervisor-reconcile.service
binaries=(gpu-mode gpu-workload-proxy gpu-operator gpu-setup)
for name in "${binaries[@]}"; do
  test ! -e "/usr/bin/$name" && test ! -L "/usr/bin/$name"
done
test ! -e "/usr/lib/systemd/user/$unit"
test ! -L "/usr/lib/systemd/user/$unit"
probe=/usr/bin/gws-ci-nvidia-fixture
test ! -e "$probe" && test ! -L "$probe"
test "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs
systemctl is-system-running --wait || test "$(systemctl is-system-running)" = degraded
account="gws-ci-$$"
! getent passwd "$account" >/dev/null
work=$(mktemp -d /tmp/gws-setup-integration.XXXXXX)
created=false
installed=()
cleanup() {
  result=$?
  trap - EXIT
  if $created; then
    loginctl terminate-user "$account" || true
    loginctl disable-linger "$account" || true
    userdel --remove "$account" || true
  fi
  for path in "${installed[@]}"; do rm -f -- "$path"; done
  rm -rf -- "$work"
  exit "$result"
}
trap cleanup EXIT
chmod 755 "$work"
dpkg-deb --extract "$package" "$work/package"
for name in "${binaries[@]}"; do
  install -o root -g root -m 755 "$work/package/usr/bin/$name" "/usr/bin/$name"
  installed+=("/usr/bin/$name")
done
install -o root -g root -m 644 "$work/package/usr/lib/systemd/user/$unit" "/usr/lib/systemd/user/$unit"
installed+=("/usr/lib/systemd/user/$unit")
cat > "$probe" <<'PROBE'
#!/bin/sh
case "$1" in
  --query-gpu=memory.free) echo 65536;;
  --query-gpu=memory.used) echo 0;;
  *) exit 1;;
esac
PROBE
chmod 755 "$probe"
installed+=("$probe")
useradd --create-home --home-dir "$work/home" --shell /bin/bash "$account"
created=true
chmod 700 "$work/home"
qualification_uid=$(id -u "$account")
loginctl enable-linger "$account"
systemctl start "user@${qualification_uid}.service"
install -m 644 "$repo/tests/desktop/setup_integration.py" "$work/setup_integration.py"
runuser -u "$account" -- env \
  HOME="$work/home" XDG_RUNTIME_DIR="/run/user/$qualification_uid" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$qualification_uid/bus" \
  python3 "$work/setup_integration.py"
