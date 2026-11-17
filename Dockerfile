# Wi-Fi Discord Bot (wifi-manage-bot)。FreeRADIUS 機上で Docker Compose で動かす。
# - 本体は Go (discordgo) で multi-stage build で静的バイナリにする
# - アカウントリング (SQLite) を読み取り専用で見る
# ユーザー/グループはホスト側の wifi-manage-bot (uid) / freerad (gid) に合わせるため
# build 時に ARG で固定する (compose.yml の .env から渡す)
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wifi-manage-bot .

FROM debian:bookworm-slim

ARG WIFI_BOT_UID=999
ARG WIFI_BOT_GID=999

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# ホストの wifi-manage-bot と同じ uid/gid でユーザーを作る (bind mount の権限を揃えるため)
RUN groupadd -g "${WIFI_BOT_GID}" wifi-manage-bot \
    && useradd -m -u "${WIFI_BOT_UID}" -g "${WIFI_BOT_GID}" wifi-manage-bot

COPY --from=build /out/wifi-manage-bot /usr/local/bin/wifi-manage-bot

USER wifi-manage-bot
WORKDIR /opt/wifi-manage-bot
CMD ["/usr/local/bin/wifi-manage-bot"]
