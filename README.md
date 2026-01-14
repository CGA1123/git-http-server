# `git-http-server`

A simple Git HTTP server that uses `git-http-backend` to host Git repositories
over HTTP for developing tools that make use of `git`.

It enables `receive-pack` by default, allowing pushes. Pushes to repositories
that don't exist will automatically be created.

Repository paths must follow the format `<namespace>/<repo>.git`.

## Install

```
go install github.com/CGA1123/git-http-server@latest
```

## Usage

```bash
git-http-server -root /path/to/repos -port 9418
```

### Flags

- `-root` - Root directory containing Git repositories (default: `.`)
- `-port` - Port to listen on (default: `9418`)
