FROM debian:12-slim

ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    openssh-client \
    curl \
    wget \
    ca-certificates \
    jq \
    make \
    unzip \
    tar \
    xz-utils \
    openssl \
    gnupg \
  && rm -rf /var/lib/apt/lists/*

# yq (YAML processor) - multi-arch
RUN curl -sSL "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${TARGETARCH}" \
    -o /usr/local/bin/yq && chmod +x /usr/local/bin/yq

# kubectl via pkgs.k8s.io v1.32
RUN curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.32/deb/Release.key \
    | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg && \
    echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /' \
    > /etc/apt/sources.list.d/kubernetes.list && \
    apt-get update && apt-get install -y --no-install-recommends kubectl \
    && rm -rf /var/lib/apt/lists/*

CMD ["/bin/bash"]
