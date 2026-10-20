# Build the Operator, including the optional Connector.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Cache module downloads independently of source changes.
RUN GOWORK=off go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# BuildKit supplies TARGETOS/TARGETARCH for each requested image platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOWORK=off go build -trimpath -ldflags='-w' -o /out/operator ./cmd

# Package the static Operator binary and CA certificates in a minimal image.
RUN GOWORK=off GOBIN=/tools go install github.com/google/go-licenses/v2@v2.0.1 && GOWORK=off /tools/go-licenses save ./cmd --ignore=arally/ --save_path=/out/licenses && mkdir -p /out/licenses
FROM scratch
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE NOTICE /licenses/
COPY --from=builder /out/licenses /licenses/dependencies/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /
COPY --from=builder /out/operator .
USER 65532:65532

ENTRYPOINT ["/operator"]
