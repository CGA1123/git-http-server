# git-http-server

A simple Git HTTP server that uses `git-http-backend` to host Git repositories over HTTP.

## Usage

```bash
git-http-server -root /path/to/repos -port 9418
```

### Flags

- `-root` - Root directory containing Git repositories (default: `.`)
- `-port` - Port to listen on (default: `9418`)

## Docker

```bash
docker run -p 9418:9418 -v /path/to/repos:/git ghcr.io/cga1123/git-http-server
```

## Building

```bash
go build -o git-http-server .
```

## Requirements

Requires `git-http-backend` to be installed on the system. This is typically included with Git.
