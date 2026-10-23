#!/bin/bash
# Runs the tests the way the Pod runs the code: the integration tests of the spawner need root, a real workerd, and ONLY the four capabilities the spawner is given.
#
#   scripts/test-in-docker.sh            all tests
#   scripts/test-in-docker.sh -run Loop  what `go test -run` takes
#
# It needs docker (use sudo if the user cannot talk to it). The test binary is built in the golang image (the same pinned digest as the Dockerfile's), and run in the supervisor's own
# image (Debian, the official workerd) as root with `--cap-drop ALL --cap-add CHOWN --cap-add KILL --cap-add SETGID --cap-add SETUID`, a read-only root file system, a memory limit
# and a process limit. `no-new-privileges` is on, as it is for every container the guest of a microVM starts. Unit tests that need neither root nor workerd run in the same container.
set -eu
cd "$(dirname "$0")/.."
GO_IMAGE="$(sed -n 's/^FROM \(golang:[^ ]*\) AS build/\1/p' Dockerfile)"
IMAGE="${IMAGE:-koedodb-functions-test}"
out="$(mktemp -d)"
trap 'rm -rf "$out"' EXIT
chmod 755 "$out"

docker build -q -t "$IMAGE" . >/dev/null
docker run --rm -v "$PWD":/src:ro -v fn-gocache:/root/.cache/go-build -v fn-gomod:/go/pkg/mod -v "$out":/out -w /src -e CGO_ENABLED=1 -e GOFLAGS=-buildvcs=false "$GO_IMAGE" \
  sh -c 'go vet ./... && for p in spawner router wdconfig residents; do go test -race -c -o /out/$p.test ./internal/$p || exit 1; done && chmod 755 /out/*'

status=0
for p in spawner router wdconfig residents; do
  echo "=== $p"
  docker run --rm --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add KILL --cap-add SETGID --cap-add SETUID \
    --read-only --security-opt no-new-privileges --tmpfs /tmp:rw,mode=1777,size=1g --memory 6g --memory-swap 6g --pids-limit 4096 \
    -v "$out/$p.test":/t/test:ro -w /tmp -e WORKERD_BIN=/sup/workerd -e EXPECT_MINIMAL_CAPS=1 \
    --entrypoint /t/test "$IMAGE" -test.v -test.timeout 20m "$@" 2>&1 | grep -v '^=== RUN\|^=== PAUSE\|^=== CONT' || status=$?
  [ "${PIPESTATUS[0]}" = 0 ] || status=1
done
exit $status
