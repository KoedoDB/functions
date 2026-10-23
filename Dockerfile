# The KoedoDB function runtime (Node): the Go supervisor, the Node runner, and Node itself. Both base images are pinned by digest (a tag is never pulled again).
#
#   docker build -t koedodb-functions .
#
# Run it as the Pod does: a non-root user, a read-only root file system, no capabilities; the bundles in /f (read-only), a tmpfs at /sockets (the sockets and scratch directories of the
# function processes) and a disk emptyDir at /tmp (the functions' own scratch space).
FROM golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/supervisor ./cmd/supervisor

# workerd: the official release binary from npm, at the version and with the integrity hashes of workerd/package-lock.json (npm ci checks them). Unmodified. It needs only glibc 2.31 or more: the
# Node image's 2.36 is enough. To update it, change workerd/package.json, run `npm install --package-lock-only --ignore-scripts` there, and read the release notes and the security advisories
# of cloudflare/workerd first (scripts/check-advisories of the KoedoDB repository reads the version).
FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205 AS workerd
WORKDIR /w
COPY workerd/package.json workerd/package-lock.json ./
RUN npm ci --ignore-scripts --no-audit --no-fund

# node:24-bookworm-slim (Node 24 LTS; 24.22.1 of 2026-10-21, a security release). Check its advisories at each start of work (scripts/check-advisories of the KoedoDB repository).
FROM node:24-bookworm-slim@sha256:55fb5f5a527ea2c5c4a95f963e48ef90aa38c7b346054fdc8ee9957a2b55d205
COPY --from=build /out/supervisor /sup/supervisor
COPY runner/runner.mjs /sup/runner.mjs
COPY --from=workerd /w/node_modules/@cloudflare/workerd-linux-64/bin/workerd /sup/workerd
# workerd reads the system's CA certificates when it first sets up TLS (for the outbound fetch of a function) and ABORTS when there are none: "No CA certificates were loaded from the system".
# The Node image has none, so the certificate bundle of the (pinned) golang image is copied. (Node itself uses its own bundled store.)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
USER 10001:10001
ENV FUNCTIONS_DIR=/f CONTROL_DIR=/sockets RUNNER=/sup/runner.mjs NODE_BIN=/usr/local/bin/node LISTEN=:9000
EXPOSE 9000
ENTRYPOINT ["/sup/supervisor"]
