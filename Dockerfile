# Splice backend: Go API + ffmpeg, serving the web UI too.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/splice ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates curl \
 && curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux -o /usr/local/bin/yt-dlp \
 && chmod +x /usr/local/bin/yt-dlp \
 && apt-get purge -y curl && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --uid 1000 splice \
 && mkdir -p /data && chown splice /data
COPY --from=build /out/splice /usr/local/bin/splice
USER splice
ENV PORT=7860 DATA_DIR=/data
EXPOSE 7860
CMD ["splice"]
