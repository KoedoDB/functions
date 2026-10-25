# The KoedoDB function runtime: the Go supervisor and workerd WITH THE KOEDODB PATCH (per-function limits: heap, CPU time, wall time, ArrayBuffer memory, subrequests; workerd/patches, docs/limits.md).
# Every base image is pinned by digest (a tag is never pulled again) and every one is Debian 13 "trixie".
#
#   workerd/build.sh            (once for each upstream release: builds workerd/workerd-linux-amd64 and writes workerd/workerd.sha256)
#   docker build -t koedodb-functions .
#
# One image, two containers of a Pod (docs/shared-vm.md): `spawner` runs as root with CAP_CHOWN, CAP_KILL, CAP_SETGID and CAP_SETUID and nothing else, `router` as user 10001 with no
# capability. Both have a read-only root file system; the spawner needs /state (its own tmpfs) and /sockets (a tmpfs shared with the router), the router only /sockets. The image has no Node,
# no npm and no shell tools of its own beyond what the base image has.
FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/supervisor ./cmd/supervisor

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
# workerd reads the system's CA certificates when it first sets up TLS (for the outbound fetch of a function) and ABORTS when there are none: "No CA certificates were loaded from the
# system". The certificates come from Debian's ca-certificates package (security updates are in the base image's sources: rebuild the image to get a new bundle; scripts/check-advisories of
# the KoedoDB repository lists the Debian advisories). The apt lists are removed again.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* /var/log/apt/* /var/log/dpkg.log /var/log/alternatives.log \
    && test -s /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/supervisor /sup/supervisor
# workerd: upstream release tag and commit in workerd/UPSTREAM, our patches in workerd/patches, built by workerd/build.sh on a build host (Bazel, about an hour: not part of `docker build`; the result
# is not in git, it is 150 MB). The checksum of the binary that was built and tested is in workerd/workerd.sha256: the build stops here if the file is any other. (The build is reproducible in its
# inputs, every download is pinned, but the binary is not bit-for-bit reproducible: the checksum names the binary that passed the tests.)
COPY --chmod=0755 workerd/workerd-linux-amd64 /sup/workerd
COPY workerd/workerd.sha256 /tmp/workerd.sha256
RUN echo "$(cat /tmp/workerd.sha256)  /sup/workerd" | sha256sum -c - && rm /tmp/workerd.sha256
# The binary links glibc and libm only (it was built on Debian 13, glibc 2.41 like this base): the build stops here if it does not run.
RUN /sup/workerd --version
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
# No USER: the Pod sets it for each container (the spawner 0, the router 10001). The mode is the argument: `spawner` or `router`.
ENV CONTROL_DIR=/sockets STATE_DIR=/state
EXPOSE 9000 9443
ENTRYPOINT ["/sup/supervisor"]
