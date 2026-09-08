FROM --platform=$BUILDPLATFORM quay.io/fedora/fedora-minimal:44 AS buildroot
ARG DNF_FLAGS="-y --setopt=install_weak_deps=False"
RUN --mount=type=cache,id=dnf,target=/var/cache/libdnf5 \
    dnf install ${DNF_FLAGS} golang

FROM buildroot as builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,id=gomod,target=/root/go/pkg/mod \
    go mod download
COPY . .
# The buildroot stage always runs on the build host's architecture, so cross
# compile via TARGETARCH rather than emulating the Go toolchain. Both binaries
# are pure Go, hence CGO_ENABLED=0. The Go build cache is content addressed and
# keyed on the target, so sharing it across architectures is safe.
ARG TARGETARCH
RUN --mount=type=cache,id=gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -o manager ./cmd/controller/ && \
    CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -o daemon ./cmd/daemon/

FROM quay.io/fedora/fedora-minimal:44
COPY --from=builder /workspace/manager /workspace/daemon /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["manager"]
