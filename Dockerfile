# The KoedoDB function runtime: the Go supervisor and the unmodified workerd. Every base image is pinned by digest (a tag is never pulled again).
#
#   docker build -t koedodb-functions .
#
# One image, two containers of a Pod (docs/shared-vm.md): `spawner` runs as root with CAP_CHOWN, CAP_KILL, CAP_SETGID and CAP_SETUID and nothing else, `router` as user 10001 with no
# capability. Both have a read-only root file system; the spawner needs /state (its own tmpfs) and /sockets (a tmpfs shared with the router), the router only /sockets. The image has no Node,
# no npm and no shell tools of its own beyond what the base image has.
FROM golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/supervisor ./cmd/supervisor

# workerd: the official release binary from npm, at the version and with the integrity hashes of workerd/package-lock.json (npm ci checks them). Unmodified. npm is only here, in a builder
# stage: only the binary is copied on. To update it, change workerd/package.json, run `npm install --package-lock-only --ignore-scripts` there, and read the release notes and the security
# advisories of cloudflare/workerd first (scripts/check-advisories of the KoedoDB repository reads the version).
FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS workerd
WORKDIR /w
COPY workerd/package.json workerd/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

FROM debian:bookworm-slim@sha256:7ff6ba31527b9e336162fa061746c366c4b2032f4d2cff2078bea43f78efe3f8
# workerd reads the system's CA certificates when it first sets up TLS (for the outbound fetch of a function) and ABORTS when there are none: "No CA certificates were loaded from the
# system". The certificates come from Debian's ca-certificates package (security updates are in the base image's sources: rebuild the image to get a new bundle; scripts/check-advisories of
# the KoedoDB repository lists the Debian advisories). The apt lists are removed again.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* /var/cache/apt/* /var/log/apt/* /var/log/dpkg.log /var/log/alternatives.log \
    && test -s /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/supervisor /sup/supervisor
COPY --from=workerd /w/node_modules/@cloudflare/workerd-linux-64/bin/workerd /sup/workerd
# The official binary needs glibc 2.31 or more (this base has 2.36): the build stops here if it does not run.
RUN /sup/workerd --version
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
# No USER: the Pod sets it for each container (the spawner 0, the router 10001). The mode is the argument: `spawner` or `router`.
ENV CONTROL_DIR=/sockets STATE_DIR=/state
EXPOSE 9000 9443
ENTRYPOINT ["/sup/supervisor"]
