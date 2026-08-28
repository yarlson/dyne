FROM node:24.14.1-bookworm-slim@sha256:b506e7321f176aae77317f99d67a24b272c1f09f1d10f1761f2773447d8da26c

ARG TARGETARCH

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates curl git openssh-client ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global "@openai/codex@0.150.1" \
    && case "${TARGETARCH}" in amd64) mise_arch=x64; mise_sha=7cd12d6002d5b3c83a89cad79023712faf2a36f9e8b2ee2061dac5135b3de0ed ;; arm64) mise_arch=arm64; mise_sha=bc2c447a7e498b0bed0a421cc2101b407fef09a3195670d35a4aa3f43cd868a1 ;; *) exit 1 ;; esac \
    && curl --fail --location --silent --show-error --output /usr/local/bin/mise "https://github.com/jdx/mise/releases/download/v2026.8.14/mise-v2026.8.14-linux-${mise_arch}" \
    && printf '%s  %s\n' "${mise_sha}" /usr/local/bin/mise | sha256sum --check \
    && chmod 0755 /usr/local/bin/mise

COPY --chmod=0755 container/agent-entrypoint /usr/local/bin/agent-entrypoint
COPY --chmod=0755 container/git-askpass /usr/local/bin/git-askpass

ENV HOME=/home/agent \
    CODEX_HOME=/home/agent/.codex \
    MISE_DATA_DIR=/home/agent/.local/share/mise \
    MISE_CACHE_DIR=/home/agent/.cache/mise \
    npm_config_cache=/home/agent/.cache/npm \
    PATH=/home/agent/.local/share/mise/shims:/home/agent/.local/bin:/usr/local/bin:/usr/bin:/bin

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/agent-entrypoint"]
CMD ["idle"]
