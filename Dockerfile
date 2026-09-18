# The build stage runs on the build host's platform and cross-compiles for
# the target platform, so a multi-platform build needs no emulation. Both
# base images are pinned by digest; Dependabot keeps the pins current.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS builder

ARG TARGETOS
ARG TARGETARCH

# The official image already sets this. It is repeated so that a base image
# change that drops it makes the build fail loudly instead of downloading a
# toolchain to satisfy go.mod.
ENV GOTOOLCHAIN=local

WORKDIR /workspace

# The module files change less often than the sources, so the download is
# cached separately from the build.
COPY go.mod go.sum ./
RUN go mod download

# .dockerignore mirrors the paths copied into the image.
COPY cmd/ cmd/
COPY internal/ internal/

# Static for the distroless base, without build paths or symbol tables.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o manager ./cmd/manager

# The runtime image holds the binary and the license files alone. The base
# already runs as its nonroot user; the USER line keeps that true should
# the base change. The source label lets a registry link the image to the
# repository.
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
LABEL org.opencontainers.image.source="https://github.com/petasus-ai/nvsentinel-capi-remediator" \
      org.opencontainers.image.licenses="Apache-2.0"
WORKDIR /
COPY LICENSE NOTICE /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
