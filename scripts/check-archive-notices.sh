#!/usr/bin/env bash
set -euo pipefail
archive=$1
for arch in amd64 arm64; do
  uuid_notice="THIRD_PARTY_NOTICES/linux_${arch}/github.com/google/uuid/LICENSE"
  text=$(tar -xOzf "$archive" "$uuid_notice")
  grep -F 'Copyright' <<< "$text" > /dev/null
  grep -F 'Redistribution and use in source and binary forms' <<< "$text" > /dev/null
  grep -F 'THIS SOFTWARE IS PROVIDED' <<< "$text" > /dev/null
  # Representative transitive notices must accompany both executables too.
  tar -tzf "$archive" | grep -E "^THIRD_PARTY_NOTICES/linux_${arch}/modernc.org/libc/.*LICENSE" > /dev/null
  tar -tzf "$archive" | grep -E "^THIRD_PARTY_NOTICES/linux_${arch}/golang.org/x/sys/.*LICENSE" > /dev/null
done
tar -xOzf "$archive" THIRD_PARTY_NOTICES/go/LICENSE | grep -F 'The Go Authors' > /dev/null
