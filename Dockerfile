FROM golang:1.26.4-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/license-core ./third_party/license-core
COPY scripts/license-core-sync.sh scripts/license-core.sha256 ./scripts/
RUN sh scripts/license-core-sync.sh --check
ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=${GOSUMDB}
RUN set -eu; \
    for attempt in 1 2 3 4 5; do \
      if go mod download && go mod verify; then exit 0; fi; \
      echo "go module download failed (attempt ${attempt}/5)" >&2; \
      sleep $((attempt * 2)); \
    done; \
    exit 1
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY authz/ ./authz/
COPY migrations/ ./migrations/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-catalog-sync ./cmd/catalog-sync

FROM alpine:3.22@sha256:d406446bc05b82480756ce8f1287964d28768c23955fa9480d2a74a2eae7a654
ARG APP_VERSION
LABEL org.opencontainers.image.version=${APP_VERSION} com.basic-platform.license.protocol="1"
RUN apk add --no-cache ca-certificates tzdata && addgroup -S settlement && adduser -S -G settlement settlement && mkdir -p /var/lib/commercial-license && chown settlement:settlement /var/lib/commercial-license
COPY --from=build /out/settlement-* /usr/local/bin/
USER settlement
EXPOSE 8085
ENTRYPOINT ["/usr/local/bin/settlement-api"]
