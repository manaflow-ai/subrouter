# Build stage: static Go binary, no CGO.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/subrouter ./cmd/subrouter

# Runtime stage: small image, non-root user, state under /var/lib/subrouter.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S subrouter \
    && adduser -S -G subrouter -h /var/lib/subrouter -u 10001 subrouter \
    && mkdir -p /var/lib/subrouter \
    && chown -R subrouter:subrouter /var/lib/subrouter
COPY --from=build /out/subrouter /usr/local/bin/subrouter
RUN ln -s /usr/local/bin/subrouter /usr/local/bin/sr \
    && ln -s /usr/local/bin/subrouter /usr/local/bin/cx

ENV HOME=/var/lib/subrouter \
    SUBROUTER_STATE_DIR=/var/lib/subrouter

USER subrouter
WORKDIR /var/lib/subrouter
VOLUME ["/var/lib/subrouter"]
EXPOSE 31415

ENTRYPOINT ["subrouter"]
CMD ["serve", "--addr", "0.0.0.0:31415", "--sessions", "/var/lib/subrouter/sessions.json"]
