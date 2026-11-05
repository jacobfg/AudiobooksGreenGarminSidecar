# syntax=docker/dockerfile-upstream:1.20
FROM --platform=$BUILDPLATFORM docker.io/golang:1.27.3 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
  go build -trimpath -ldflags="-s -w" -o /usr/local/bin/sidecar ./cmd/sidecar

# makes minimal image, but keeps the ffmpeg binaries as a cachable layer
FROM scratch
COPY --from=docker.io/mwader/static-ffmpeg:9.0.2-1 /ffmpeg /ffprobe /usr/local/bin/
COPY --from=build --parents /etc/ssl/certs/ca-certificates.crt /usr/local/bin/sidecar /
WORKDIR /app
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/sidecar"]
