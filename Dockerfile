# Stage 1: compile the plugin binary
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin

# Stage 2: runtime with tt-smi baked in (no host mounts needed)
FROM ubuntu:22.04
RUN apt-get update && \
    apt-get install -y --no-install-recommends python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

# Install tt-smi from local source (same version running on the T3K nodes).
# Build with: docker buildx build --build-context tt-smi=/home/ubuntu/tt-smi ...
COPY --from=tt-smi . /tmp/tt-smi/
RUN pip3 install --no-cache-dir tomli && \
    pip3 install --no-cache-dir --no-build-isolation /tmp/tt-smi/ && \
    rm -rf /tmp/tt-smi/

COPY --from=builder /wh-dra-kubelet-plugin /usr/local/bin/wh-dra-kubelet-plugin

ENTRYPOINT ["/usr/local/bin/wh-dra-kubelet-plugin"]
