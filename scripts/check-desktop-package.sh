#!/usr/bin/env bash
set -euo pipefail
package="${1:?usage: check-desktop-package.sh package.deb}"
test "$(dpkg-deb --field "$package" Package)" = gpu-workload-supervisor
case "$(dpkg-deb --field "$package" Architecture)" in amd64|arm64) ;; *) exit 1;; esac
depends="$(dpkg-deb --field "$package" Depends)"
[[ "$depends" == *'gnome-shell (>= 50)'* && "$depends" == *'gnome-shell (<< 51)'* ]]
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
dpkg-deb --extract "$package" "$root"
dpkg-deb --control "$package" "$root/DEBIAN"
version="$(dpkg-deb --field "$package" Version)"
for binary in gpu-mode gpu-workload-proxy gpu-operator gpu-setup; do
  test -x "$root/usr/bin/$binary"
  embedded_release="$(go version -m "$root/usr/bin/$binary" | sed -n 's/.*deployment.Release=\([^ " ]*\).*/\1/p')"
  test -n "$embedded_release"
  test "$embedded_release" != dev
  test "$embedded_release" = "$version"
done
for file in extension.js metadata.json model.js contract.js transport.js framing.js dialogs.js; do
  test -s "$root/usr/share/gnome-shell/extensions/gpu-workload-supervisor@local/$file"
done
for file in usr/share/gpu-workload-supervisor/setup.js usr/share/gpu-workload-supervisor/review.mjs usr/share/applications/gpu-workload-supervisor-setup.desktop usr/lib/systemd/user/gpu-workload-supervisor-reconcile.service usr/share/doc/gpu-workload-supervisor/copyright; do
  test -s "$root/$file"
done
# Installation/removal must never run code against logged-in user state.
for script in preinst postinst prerm postrm; do
  test ! -e "$root/DEBIAN/$script"
done
node -e 'const fs=require("node:fs");const m=JSON.parse(fs.readFileSync(process.argv[1]));if(JSON.stringify(m["shell-version"])!==JSON.stringify(["50"]))process.exit(1)' "$root/usr/share/gnome-shell/extensions/gpu-workload-supervisor@local/metadata.json"
