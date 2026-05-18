# SFU (Go)

A simple Selective Forwarding Unit (SFU) implemented in Go with a minimal web UI.

This repository contains a small WebRTC SFU server and static frontend pages to create and join rooms.

Files of interest:
- `main.go` - application entrypoint and HTTP server
- `index.html` / `room.html` - minimal web UI
- `db/redis.go` - Redis integration
- `Dockerfile` - containerization

Prerequisites
- Go 1.17+ installed
- (Optional) Redis if you use the Redis-backed features in `db/redis.go`

Build & Run (local)

1. Fetch dependencies:

```
go mod tidy
```

2. Run directly:

```
go run main.go
```

Or build and run the binary:

```
go build -o sfu
./sfu
```

Docker

Build the image:

```
docker build -t sfu .
```

Run the container (example binding port 8080):

```
docker run -p 8080:8080 sfu
```

Configuration
- The server listens on the port configured in `main.go` (commonly `:8080`).
- If the project uses Redis, configure connection settings in `db/redis.go` or via environment variables as implemented.

Usage
- Open your browser to `http://localhost:8080` (or the configured port) to access the UI.
- Use the provided `index.html` to create or join a room; `room.html` is the room interface.

Contributing
- Bug reports and PRs are welcome. Keep changes small and focused.

License
- This project does not include a license file. Add one if you intend to publish.
