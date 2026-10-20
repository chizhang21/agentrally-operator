FROM node:26-bookworm-slim

ARG CODEXAPP_VERSION=latest
ARG CODEX_VERSION=latest

LABEL org.opencontainers.image.title="Codex Mobile" \
      org.opencontainers.image.description="Third-party container build of friuns2/codex-mobile with Codex CLI" \
      org.opencontainers.image.url="https://github.com/friuns2/codex-mobile"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git python3 make g++ \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g "@brutalstrikedevs/codexapp@${CODEXAPP_VERSION}" "@openai/codex@${CODEX_VERSION}" \
    && node -e "require('/usr/local/lib/node_modules/@brutalstrikedevs/codexapp/node_modules/node-pty')" \
    && mkdir -p /licenses/codex /workspace \
    && test -s /usr/local/lib/node_modules/@brutalstrikedevs/codexapp/LICENSE \
    && codex_version="$(node -p "require('/usr/local/lib/node_modules/@openai/codex/package.json').version")" \
    && curl -fsSL --retry 3 "https://raw.githubusercontent.com/openai/codex/rust-v${codex_version}/LICENSE" -o /licenses/codex/LICENSE \
    && curl -fsSL --retry 3 "https://raw.githubusercontent.com/openai/codex/rust-v${codex_version}/NOTICE" -o /licenses/codex/NOTICE \
    && npm cache clean --force \
    && chown 1000:1000 /workspace

ENV HOME=/workspace \
    CODEX_HOME=/workspace/.codex \
    SHELL=/bin/bash \
    CODEXUI_SANDBOX_MODE=workspace-write \
    CODEXUI_APPROVAL_POLICY=on-request
WORKDIR /workspace
USER 1000:1000
EXPOSE 5900
ENTRYPOINT ["codexapp"]
CMD ["--port", "5900", "--no-open", "--no-tunnel", "--no-login"]
