FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /zirox ./cmd/zirox

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 zirox && mkdir /data && chown zirox /data
COPY --from=build /zirox /usr/local/bin/zirox
USER zirox
ENV ZIROX_DATA_DIR=/data ZIROX_LISTEN=:9116
VOLUME /data
EXPOSE 9116
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:9116/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/zirox"]
