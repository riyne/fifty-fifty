# Config is baked in rather than bind-mounted so the stack needs no Docker Desktop
# file-sharing setup. After editing config.yaml run: make up (it passes --build).
FROM ghcr.io/dexidp/dex:v2.46.0
COPY config.yaml /etc/dex/config.yaml
CMD ["dex", "serve", "/etc/dex/config.yaml"]
