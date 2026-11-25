FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/solitudeit/nfr/internal/version.Version=${VERSION}" \
      -o /out/nfrs ./cmd/nfrs && \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/solitudeit/nfr/internal/version.Version=${VERSION}" \
      -o /out/nfrc ./cmd/nfrc

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 nfr
COPY --from=build /out/nfrs /usr/local/bin/nfrs
COPY --from=build /out/nfrc /usr/local/bin/nfrc
# State, certificates, secret key and generated configs live here.
VOLUME /var/lib/nfr
# 8443 control + console, 80/443 ingress, 20000-29999 public tunnel ports.
EXPOSE 8443 80 443
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8443/.well-known/nfr >/dev/null 2>&1 || wget --no-check-certificate -qO- https://127.0.0.1:8443/.well-known/nfr >/dev/null 2>&1 || exit 1
# nfrs runs in the foreground by default (no daemon). To run the client from the
# same image, override entrypoint with `--entrypoint nfrc`.
ENTRYPOINT ["nfrs"]
CMD ["-c", "/var/lib/nfr/server.toml"]
