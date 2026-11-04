# Shared build dependencies for the Operator and Connector images.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS builder
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

RUN GOWORK=off GOBIN=/tools go install github.com/google/go-licenses/v2@v2.0.1

FROM builder AS operator-build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOWORK=off go build -trimpath -ldflags='-w' -o /out/operator ./cmd
RUN GOWORK=off /tools/go-licenses save ./cmd --ignore=agentrally/ --save_path=/out/licenses && mkdir -p /out/licenses

FROM builder AS connector-build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} GOWORK=off go build -trimpath -ldflags='-w' -o /out/connector ./cmd/connector
RUN GOWORK=off /tools/go-licenses save ./cmd/connector --ignore=agentrally/ --save_path=/out/licenses && mkdir -p /out/licenses

FROM scratch AS runtime
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY LICENSE NOTICE /licenses/
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
WORKDIR /
USER 65532:65532

FROM runtime AS connector
COPY --from=connector-build /out/licenses /licenses/dependencies/
COPY --from=connector-build /out/connector /connector
ENTRYPOINT ["/connector"]

# Keep the Operator as the default target for plain container builds.
FROM runtime AS operator
COPY --from=operator-build /out/licenses /licenses/dependencies/
COPY --from=operator-build /out/operator /operator
ENTRYPOINT ["/operator"]
