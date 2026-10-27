FROM --platform=$BUILDPLATFORM golang:1.26.0-alpine AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
    -ldflags="-s -w -X teo-ai/internal/buildinfo.Version=${VERSION} -X teo-ai/internal/buildinfo.Commit=${COMMIT} -X teo-ai/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/teo-ai-gateway ./cmd/teo-ai-gateway

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/teo-ai-gateway /usr/local/bin/teo-ai-gateway
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/teo-ai-gateway"]
