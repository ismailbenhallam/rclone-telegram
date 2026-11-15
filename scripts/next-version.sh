#!/usr/bin/env bash
# Prints the next release tag.
#
# Releases follow the rclone version they are built on. MAJOR.MINOR is
# always rclone's. The patch number is rclone's too, unless this project
# already released that number (a backend fix), then it is the next free
# one:
#   rclone v1.76.0                  -> v1.76.0
#   backend fix, still rclone 1.76.0 -> v1.76.1
#   rclone v1.76.1                  -> v1.76.2 (v1.76.1 is taken)
#   rclone v1.76.3                  -> v1.76.3
#   rclone v1.77.0                  -> v1.77.0
set -euo pipefail

rclone=$(go list -m -f '{{.Version}}' github.com/rclone/rclone)
rclone=${rclone#v}
mm=$(echo "$rclone" | cut -d. -f1,2)
patch=$(echo "$rclone" | cut -d. -f3 | cut -d- -f1)
last=$(git tag -l "v${mm}.*" --sort=-v:refname | head -n1)

if [ -n "$last" ] && [ $(( ${last##*.} + 1 )) -gt "$patch" ]; then
  patch=$(( ${last##*.} + 1 ))
fi
echo "v${mm}.${patch}"
