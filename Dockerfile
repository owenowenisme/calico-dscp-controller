FROM golang:1.24 as builder

WORKDIR /app

# Copy go mod files first
COPY go.mod go.sum ./

# Download dependencies - this layer will be cached if the go.mod and go.sum files haven't changed
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o dscp-controller .

FROM alpine:latest

WORKDIR /

# Install required dependencies including iptables
RUN apk add --no-cache \
    ca-certificates \
    libc6-compat \
    iptables \
    ip6tables

COPY --from=builder /app/dscp-controller .

# Set proper permissions
RUN chmod +x /dscp-controller

ENTRYPOINT ["/dscp-controller"]