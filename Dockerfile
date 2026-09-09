FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# Only ver2 enters the build context, so editing frozen v1 code cannot
# invalidate this layer.
COPY ver2 ./ver2
# Which build is running is a question that has already blocked a diagnosis:
# a fix was pushed, the container was not rebuilt, and the old behaviour was
# read as the fix not working. The commit is stamped in so the log can say.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/nvtweb ./ver2/cmd/nvtweb

FROM alpine:3.20
# Fonts are not optional here. libass draws nothing at all when it cannot find
# a face for the text — it warns and carries on, ffmpeg exits zero, and an hour
# of encoding produces a file with no subtitles in it and no error anywhere.
# The base image ships no fonts whatsoever, so burning Korean subtitles into a
# picture was never going to work. Noto CJK covers Korean and Japanese, which
# is what these files are.
RUN apk add --no-cache ffmpeg ca-certificates tzdata fontconfig font-noto-cjk
COPY --from=build /out/nvtweb /usr/local/bin/nvtweb
ENV NVT2_SOURCE_DIR=/media \
    NVT2_OUTPUT_DIR=/output \
    NVT2_STATE_DIR=/state \
    NVT2_LISTEN=:8080
VOLUME ["/output", "/state"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nvtweb"]
