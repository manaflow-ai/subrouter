# syntax=docker/dockerfile:1.7

FROM golang:1.24-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/subrouter ./cmd/subrouter && \
    mkdir -p /out/state

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/subrouter /usr/local/bin/subrouter
COPY --from=build --chown=65532:65532 /out/state /var/lib/subrouter

ENV HOME=/var/lib/subrouter \
    SUBROUTER_STATE_DIR=/var/lib/subrouter \
    GOMEMLIMIT=192MiB \
    GOMAXPROCS=2

USER 65532:65532
EXPOSE 31415
VOLUME ["/var/lib/subrouter"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/subrouter", "probe", "--url", "http://127.0.0.1:31415"]
ENTRYPOINT ["/usr/local/bin/subrouter"]
CMD ["serve", "--addr", "127.0.0.1:31415", "--sessions", "/var/lib/subrouter/sessions.json"]
