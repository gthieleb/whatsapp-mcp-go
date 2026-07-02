# Build the MCP server binary
FROM golang:1.25.7 AS builder

ARG TARGETARCH
ARG TARGETOS
ARG BINARY_NAME=whatsapp-mcp

WORKDIR /project

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GO111MODULE=on go build -a -o ${BINARY_NAME} main.go

# Provide a statically linked tar binary (busybox:musl) for media download handling.
# busybox:stable is NOT used because it ships a dynamically linked binary
# (NEEDED: libm.so.6, libresolv.so.2, libc.so.6) that fails in static-distroless.
# This is a workaround until MCP streaming or native media download is implemented.
FROM busybox:musl AS tar-provider

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /project
COPY --from=builder /project/whatsapp-mcp .
COPY --from=tar-provider /bin/tar /usr/bin/tar

USER 65532:65532

ENTRYPOINT ["/project/whatsapp-mcp"]
