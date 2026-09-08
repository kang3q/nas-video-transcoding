FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Only ver2 enters the build context, so editing frozen v1 code cannot
# invalidate this layer.
COPY ver2 ./ver2
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nvtweb ./ver2/cmd/nvtweb

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /out/nvtweb /usr/local/bin/nvtweb
ENV NVT2_SOURCE_DIR=/media \
    NVT2_OUTPUT_DIR=/output \
    NVT2_STATE_DIR=/state \
    NVT2_LISTEN=:8080
VOLUME ["/output", "/state"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nvtweb"]
