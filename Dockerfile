FROM golang:1.26.0-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/teo-ai-gateway ./cmd/teo-ai-gateway

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 10001 teo && adduser -D -u 10001 -G teo teo
COPY --from=build /out/teo-ai-gateway /usr/local/bin/teo-ai-gateway
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/teo-ai-gateway"]
