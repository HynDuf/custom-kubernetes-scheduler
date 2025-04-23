# Use the official Golang image to create a build artifact.
FROM golang:1.23-alpine AS builder
WORKDIR /app

# Copy only the go.mod and go.sum first for better caching
COPY ./go.mod ./go.sum ./scheduler/
WORKDIR /app/scheduler
RUN go mod download

# Now copy the rest of the source code
COPY ./ ./

# Build the Go app (statically linked)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /scheduler .

# Use a minimal base image for the final container
FROM alpine:latest

# Copy the statically linked binary
COPY --from=builder /scheduler /scheduler

# Run the binary
ENTRYPOINT ["/scheduler"]
