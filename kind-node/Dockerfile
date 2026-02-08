# Custom kind node image with runc v1.4.0 and containerd with CDI 1.1.0 support
# Build from source to ensure netDevices support
FROM kindest/node:v1.35.0

# Install build dependencies
RUN apt-get update && apt-get install -y \
    build-essential \
    libseccomp-dev \
    pkg-config \
    git \
    libbtrfs-dev \
    && rm -rf /var/lib/apt/lists/*

# Detect architecture and install appropriate Go version
ARG TARGETARCH
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then GOARCH="amd64"; \
    elif [ "$ARCH" = "aarch64" ]; then GOARCH="arm64"; \
    else echo "Unsupported arch: $ARCH" && exit 1; fi && \
    curl -fsSL "https://go.dev/dl/go1.25.0.linux-${GOARCH}.tar.gz" | tar -C /usr/local -xzf -
ENV PATH="/usr/local/go/bin:${PATH}"

# Clone and build runc v1.4.0
RUN git clone --branch v1.4.0 --depth 1 https://github.com/opencontainers/runc.git /tmp/runc && \
    cd /tmp/runc && \
    make && \
    cp runc /usr/local/sbin/runc && \
    rm -rf /tmp/runc

# Clone and build containerd from main branch (already has CDI v1.1.0)
RUN git clone --branch v2.2.1 --depth 1 https://github.com/containerd/containerd.git /tmp/containerd && \
    cd /tmp/containerd && \
    make binaries && \
    cp bin/containerd /usr/local/bin/containerd && \
    cp bin/containerd-shim-runc-v2 /usr/local/bin/ && \
    cp bin/ctr /usr/local/bin/ && \
    rm -rf /tmp/containerd /usr/local/go /root/go

# Verify the installations
RUN /usr/local/sbin/runc --version && /usr/local/bin/containerd --version
