# syntax=docker/dockerfile:1
#
# live-ninja Nova Sonic media bridge (plan.md M12, FR-VE-02).
#
# A single small arm64 Fargate service that holds the client WebSocket and
# the Bedrock InvokeModelWithBidirectionalStream HTTP/2 stream for a Nova
# Sonic session's life (cmd/nova-bridge). Distroless static + a CGO-free Go
# binary → a tiny (~15-20 MB) non-root image with no shell and no package
# surface. CA certs (for HTTPS to Bedrock, the JWKS endpoint, and the
# transcript/tools API) ship in the distroless static base.
#
# BUILD CONTEXT IS THE REPO ROOT — the Go build needs the whole module:
#   docker buildx build --platform linux/arm64 \
#     -f containers/nova-bridge/Dockerfile -t nova-bridge .
# CI builds/pushes it to ECR path-filtered to cmd/nova-bridge/**,
# internal/**, and containers/nova-bridge/** (deploy.md: no local deploys).
#
# The build stage runs natively on the builder ($BUILDPLATFORM) and
# cross-compiles to $TARGETARCH, so an arm64 image builds fast on an amd64
# runner without QEMU emulating the Go toolchain.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Module graph first for layer caching — deps only re-download when go.mod/
# go.sum change, not on every source edit.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped, reproducible (-trimpath) binary. CGO off → no libc
# dependency, so it runs on the distroless static base.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-arm64} \
    go build -trimpath -ldflags "-s -w" -o /out/nova-bridge ./cmd/nova-bridge

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

# The bridge listens on 8080 (override with PORT); the ALB target group
# health-checks GET /healthz. Distroless has no shell, so there is no
# container HEALTHCHECK — the ALB is the health authority.
EXPOSE 8080

COPY --from=build /out/nova-bridge /usr/local/bin/nova-bridge

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/nova-bridge"]
