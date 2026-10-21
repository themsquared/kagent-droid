# Factory Droid as a kagent BYO agent (DevOps / platform engineering toolbox).
# The A2A front is Go: Substrate (gVisor) did not restore a Python process.
FROM golang:1.26-trixie AS front
WORKDIR /src
COPY front/go.mod front/go.sum ./
RUN go mod download
COPY front/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /droid-harness .

FROM debian:trixie-slim

ARG TARGETARCH
ARG DROID_VERSION=0.242.0
ARG KUBECTL_VERSION=v1.34.1
ARG HELM_VERSION=v3.19.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl git jq ripgrep openssh-client \
 && rm -rf /var/lib/apt/lists/*

# droid CLI, checksum-verified like Factory's installer. It runs only during a turn.
RUN set -eux; \
    case "$TARGETARCH" in arm64) a=arm64 ;; amd64) a=x64 ;; *) exit 1 ;; esac; \
    base="https://downloads.factory.ai/factory-cli/releases/${DROID_VERSION}/linux/$a/droid"; \
    curl -fsSL -o /usr/local/bin/droid "$base"; \
    echo "$(curl -fsSL "$base.sha256")  /usr/local/bin/droid" | sha256sum -c -; \
    chmod +x /usr/local/bin/droid

# platform tools the Droid can call from its shell
RUN set -eux; \
    curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256")  /usr/local/bin/kubectl" | sha256sum -c -; \
    chmod +x /usr/local/bin/kubectl; \
    curl -fsSL -o /tmp/helm.tgz "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    echo "$(curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz.sha256sum" | awk '{print $1}')  /tmp/helm.tgz" | sha256sum -c -; \
    tar -xzf /tmp/helm.tgz -C /tmp; mv /tmp/linux-${TARGETARCH}/helm /usr/local/bin/helm; rm -rf /tmp/helm.tgz /tmp/linux-${TARGETARCH}

COPY --from=front /droid-harness /usr/local/bin/droid-harness

ENV DROID_AUTONOMY=high
EXPOSE 80 8081
ENTRYPOINT ["/usr/local/bin/droid-harness"]
