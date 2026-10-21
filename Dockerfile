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

# node:24-bookworm-slim (Node 24 LTS). Check its advisories at each start of work (scripts/check-advisories of the KoedoDB repository).
FROM node:24-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20
COPY --from=build /out/supervisor /sup/supervisor
COPY runner/runner.mjs /sup/runner.mjs
USER 10001:10001
ENV FUNCTIONS_DIR=/f CONTROL_DIR=/sockets RUNNER=/sup/runner.mjs NODE_BIN=/usr/local/bin/node LISTEN=:9000
EXPOSE 9000
ENTRYPOINT ["/sup/supervisor"]
