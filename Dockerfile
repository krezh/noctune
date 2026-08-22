# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/noctune ./cmd/noctune

FROM alpine:latest AS runtime
RUN apk add --no-cache ffmpeg python3 py3-pip ca-certificates && \
    pip3 install --no-cache-dir --break-system-packages yt-dlp && \
    addgroup -S noctune && adduser -S noctune -G noctune

COPY --from=build /out/noctune /usr/local/bin/noctune

USER noctune
EXPOSE 8080
ENTRYPOINT ["noctune"]
