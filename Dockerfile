FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod ./
COPY main.go ./

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o git-http-server .

FROM alpine:3.20

LABEL org.opencontainers.image.source=https://github.com/CGA1123/git-http-server
LABEL org.opencontainers.image.description="Simple Git HTTP server using git-http-backend"
LABEL org.opencontainers.image.licenses=MIT

RUN apk add --no-cache git git-daemon

COPY --from=builder /build/git-http-server /usr/local/bin/git-http-server

EXPOSE 9418

ENTRYPOINT ["/usr/local/bin/git-http-server"]
CMD ["-root", "/git", "-port", "9418"]
