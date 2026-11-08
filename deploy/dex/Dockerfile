# Config is baked in rather than bind-mounted so the stack needs no Docker Desktop
# file-sharing setup. After editing config.yaml run: make up (it passes --build).
#
# v2.46.1, not v2.46.0 or earlier ones, and with POSTGRES storage (config.yaml), because of a bug
# found in M10.21: with in-memory storage (and with SQLite) a refresh-token request with a valid
# token never answers and Dex stays wedged (storage/memory UpdateRefreshToken calls
# GetOfflineSessions while holding the lock it takes). v2.45.1 refreshes but has no
# oauth2.pkce setting (10.22). Dex v2.46.0 and v2.46.1 with Postgres storage do both.
FROM ghcr.io/dexidp/dex:v2.46.1
COPY config.yaml /etc/dex/config.yaml
CMD ["dex", "serve", "/etc/dex/config.yaml"]
