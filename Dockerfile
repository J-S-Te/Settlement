FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-migrate ./cmd/migrate \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/settlement-catalog-sync ./cmd/catalog-sync

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S settlement && adduser -S -G settlement settlement
COPY --from=build /out/settlement-* /usr/local/bin/
USER settlement
EXPOSE 8085
ENTRYPOINT ["/usr/local/bin/settlement-api"]
