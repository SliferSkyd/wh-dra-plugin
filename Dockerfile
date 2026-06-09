# Stage 1: compile the plugin binary
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin && \
    CGO_ENABLED=0 GOOS=linux go build -o /wh-node-labeler ./cmd/wh-node-labeler

# Stage 2: runtime with tt-smi installed from PyPI
FROM ubuntu:22.04
RUN apt-get update && \
    apt-get install -y --no-install-recommends python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

# Pin to the version running on the T3K nodes. Update this when upgrading tt-smi.
RUN pip3 install --no-cache-dir tt-smi

COPY --from=builder /wh-dra-kubelet-plugin /usr/local/bin/wh-dra-kubelet-plugin
COPY --from=builder /wh-node-labeler /usr/local/bin/wh-node-labeler

ENTRYPOINT ["/usr/local/bin/wh-dra-kubelet-plugin"]
