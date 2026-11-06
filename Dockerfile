# syntax=docker/dockerfile:1
FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
# Optional trust mount for the managed cloud HTTPS proxy; TLS stays enabled.
RUN --mount=type=secret,id=proxy_ca \
    if [ -f /run/secrets/proxy_ca ]; then export SSL_CERT_FILE=/run/secrets/proxy_ca; fi; \
    go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags='-s -w' -o /bot ./cmd/bot
RUN CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -ldflags='-s -w' -o /vk-session ./cmd/vk-session
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bot /app/bot
COPY --from=build /vk-session /app/vk-session
EXPOSE 8000
ENTRYPOINT ["/app/bot"]
