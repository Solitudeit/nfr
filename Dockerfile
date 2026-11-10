FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/solitudeit/nfr/internal/version.Version=${VERSION}" \
      -o /out/nfrs ./cmd/nfrs

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 nfr
COPY --from=build /out/nfrs /usr/local/bin/nfrs
# State, certificates and the generated config live here.
VOLUME /var/lib/nfr
# 8443 control + console, 80/443 ingress, 20000-29999 public tunnel ports.
EXPOSE 8443 80 443
# Running as root is the simplest way to bind 80/443; drop privileges by
# publishing only high ports and setting control_listen/http_listen instead.
ENTRYPOINT ["nfrs"]
CMD ["-c", "/var/lib/nfr/server.toml"]
