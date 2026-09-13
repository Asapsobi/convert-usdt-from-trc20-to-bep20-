# Shared build for every Go module in this repo (ledger/, depositwatcher/,
# screening/, energybroker/, dispatcher/, s1/, gateway/, proofrun/,
# opsconsole/) -- one Dockerfile, parametrized by build args, rather than
# nine near-identical copies.
# Written for docs/03-build/mvp-proof-run-plan.md's own stack; NOT exercised
# against a real Docker daemon while writing it (this session's own
# environment has no Docker installed) -- verify `docker compose build`
# succeeds before relying on this for the actual proof run.
#
# MODULE_DIR: the module's own directory, e.g. "ledger", "dispatcher".
# SERVER_BIN: the cmd/ subdirectory under MODULE_DIR to build as the
#   server binary, e.g. "ledgerd", "dispatchd".
# HAS_MIGRATE: "true" for every module with its own cmd/migrate (all of
#   them except proofrun and opsconsole, which own no database).

ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-alpine AS build
ARG MODULE_DIR
ARG SERVER_BIN
ARG HAS_MIGRATE=true

RUN apk add --no-cache git
WORKDIR /src

COPY ${MODULE_DIR}/go.mod ${MODULE_DIR}/go.sum ./
RUN go mod download

COPY ${MODULE_DIR}/ ./
RUN CGO_ENABLED=0 go build -o /out/server ./cmd/${SERVER_BIN}
# A real migrate binary for every module that owns a database, or (for
# proofrun, which owns none) a no-op placeholder -- so the final stage's
# COPY --from=build /out/migrate below always has something to copy,
# without needing BuildKit-only conditional COPY syntax.
RUN if [ "$HAS_MIGRATE" = "true" ]; then \
      CGO_ENABLED=0 go build -o /out/migrate ./cmd/migrate; \
    else \
      printf '#!/bin/sh\nexit 0\n' > /out/migrate && chmod +x /out/migrate; \
    fi

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/server /usr/local/bin/server
COPY --from=build /out/migrate /usr/local/bin/migrate
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENTRYPOINT ["docker-entrypoint.sh"]
