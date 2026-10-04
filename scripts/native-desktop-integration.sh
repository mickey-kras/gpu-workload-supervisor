#!/usr/bin/env bash
# Run only on a disposable host: Transport intentionally uses a fixed executable.
set -euo pipefail
if [[ ${1:-} != --isolated-test-host || ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted ]]; then
    echo 'Requires --isolated-test-host on a GitHub-hosted Actions runner.' >&2
    exit 1
fi
if [[ $(id -u) != 0 ]]; then
    echo 'Run with sudo, preserving GITHUB_ACTIONS and RUNNER_ENVIRONMENT.' >&2
    exit 1
fi
for executable in gjs python3 timeout; do
    command -v "$executable" >/dev/null
done
operator=/usr/bin/gpu-operator
if [[ -e $operator || -L $operator ]]; then
    echo 'Refusing to replace an existing gpu-operator.' >&2
    exit 1
fi
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture_dir=$(mktemp -d)
export GPU_NATIVE_FIXTURE_DIR=$fixture_dir
cleanup() {
    if [[ -L $operator && $(readlink -- "$operator") == "$fixture_dir/operator.py" ]]; then
        rm -- "$operator"
    fi
    rm -rf -- "$fixture_dir"
}
trap cleanup EXIT
install -m 755 "$root/clients/gnome/tests/operator-fixture.py" "$fixture_dir/operator.py"
# ln refuses a path created after the initial check; never overwrite it.
ln -s -- "$fixture_dir/operator.py" "$operator"
timeout 115s gjs -m "$root/clients/gnome/tests/transport.gjs.js"
