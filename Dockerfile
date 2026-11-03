FROM docker.io/mwader/static-ffmpeg:9.0.2-1 AS ffmpeg

FROM --platform=$BUILDPLATFORM docker.io/golang:1.27.3 AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /sidecar ./cmd/sidecar

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY --from=ffmpeg /ffprobe /usr/local/bin/ffprobe
COPY --from=build /sidecar /usr/local/bin/sidecar
WORKDIR /app
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/sidecar"]
