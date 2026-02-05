# Use the official Golang image to create a build artifact.
FROM golang:alpine AS builder

# Set the working directory inside the container.
WORKDIR /app

# Copy go mod and sum files.
COPY go.mod go.sum ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed.
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container.
COPY . .

# Build the Go app.
RUN go build -o main .

# Start a new stage to keep the final image clean and small.
FROM alpine:latest

WORKDIR /root/

# Copy the pre-built binary file from the previous stage.
COPY --from=builder /app/main .

# Copy static files required for the application to run.
COPY --from=builder /app/index.html .
COPY --from=builder /app/room.html .
COPY --from=builder /app/style.css .

# Expose port 8080 to the outside world.
EXPOSE 8080

# Command to run the executable
CMD ["./main"]
