ARG GO_BUILDER_IMAGE=quay.io/hummingbird/go:1.26.8-builder
ARG CORE_RUNTIME_IMAGE=quay.io/hummingbird/core-runtime:2.43


FROM ${GO_BUILDER_IMAGE} AS deps
# We pull required dependencies and overlay them
# on top of the runtime image
ARG DNF_FLAGS="-y --setopt=install_weak_deps=False --nodocs"
RUN --mount=type=cache,id=dnf,target=/var/cache/libdnf5 \
    mkdir -p /staged &&\
    dnf install -y \
        --use-host-config \
        --installroot=/staged \
        util-linux-core && \
    # Remove metadata and manpages
    rm -rf /staged/var/lib/dnf \
        /staged/var/log/* \
        /staged/var/lib/rpm \
        /staged/usr/share/{man,doc,locale}

FROM ${GO_BUILDER_IMAGE} AS builder

ARG VERSION=dev
ARG GIT_COMMIT=unknown
WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,id=gomod,target=/root/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,id=gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    LDFLAGS="-X github.com/bootc-dev/bootc-operator/internal/version.Version=${VERSION} -X github.com/bootc-dev/bootc-operator/internal/version.GitCommit=${GIT_COMMIT}" && \
    go build -ldflags "${LDFLAGS}" -o manager ./cmd/controller/ && \
    go build -ldflags "${LDFLAGS}" -o daemon ./cmd/daemon/

FROM ${CORE_RUNTIME_IMAGE}
COPY --from=deps /staged/usr /usr
COPY --from=builder /workspace/manager /workspace/daemon /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["manager"]
