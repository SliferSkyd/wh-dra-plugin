FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /wh-dra-kubelet-plugin ./cmd/wh-dra-kubelet-plugin

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /wh-dra-kubelet-plugin /wh-dra-kubelet-plugin
ENTRYPOINT ["/wh-dra-kubelet-plugin"]
