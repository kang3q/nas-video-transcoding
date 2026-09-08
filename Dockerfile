FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nvt ./cmd/nvt

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /out/nvt /usr/local/bin/nvt
ENV NVT_MEDIA_DIR=/media \
    NVT_CACHE_DIR=/cache \
    NVT_LISTEN=:8080
VOLUME ["/cache"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nvt"]
