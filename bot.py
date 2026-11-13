#!/usr/bin/env python3
"""Wi-Fi の接続状況の確認と証明書の発行を行う Discord Bot (wifi-bot リポジトリ)。

FreeRADIUS が SQLite に書き込むアカウントリング
(Start / Interim-Update / Stop) を読み、

- /access-log: acctstoptime が NULL (切断されていない) セッションの部員一覧。
  連携済みの部員は Discord のメンションで表示し、未連携の部員は username で表示する。
  各部員の接続開始時刻と、最後の利用 (誰が・いつから〜いつ) も出す
- /wifi-cert: 実行者の Wi-Fi クライアント証明書を発行して送る。
  Apple 端末 (iOS / macOS / iPadOS) 向けに .mobileconfig (Wi-Fi 設定込みのプロファイル)、
  それ以外の OS 向けに従来どおり .p12 とパスワードを添える。
  誰に発行してよいかの判定 (Discord 連携済み + 部員トークン) は Authentik で行い、
  署名は root helper (wifi-cert-issue) がローカル CA で行う。返信は ephemeral
"""

import asyncio
import base64
import io
import json
import os
import plistlib
import sqlite3
import subprocess
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import datetime
from typing import Literal
from zoneinfo import ZoneInfo

import discord
from discord import app_commands

JST = ZoneInfo("Asia/Tokyo")

DB_PATH = os.environ["WIFI_RADIUS_DB"]
GUILD_ID = int(os.environ["WIFI_DISCORD_GUILD_ID"])
# /wifi-cert でユーザーを Discord ID から特定するための Authentik API (wifi-provision のトークン)
AUTHENTIK_URL = os.environ.get("WIFI_AUTHENTIK_URL", "").rstrip("/")
AUTHENTIK_API_TOKEN = os.environ.get("WIFI_AUTHENTIK_API_TOKEN", "")
AUTHENTIK_SOURCE_SLUG = os.environ.get("WIFI_AUTHENTIK_SOURCE_SLUG", "discord")
# 部員トークン (wifi-<username>) の名前の prefix (FreeRADIUS 側の部員管理と一致させる)
WIFI_TOKEN_PREFIX = os.environ.get("WIFI_TOKEN_PREFIX", "wifi-")
# クライアント証明書の発行 helper (/wifi-cert)。root のみが読める CA 秘密鍵で署名する
CERT_HELPER = os.environ.get("WIFI_CERT_HELPER", "/usr/local/sbin/wifi-cert-issue")
# helper が発行した p12 を置くディレクトリ (Bot が読み取って消す)
CERT_ISSUED_DIR = os.environ.get("WIFI_CERT_ISSUED_DIR", "/opt/wifi-bot/issued")
# .mobileconfig (Apple 端末用プロファイル) の Wi-Fi 設定。SSID と RADIUS サーバー証明書の検証名
WIFI_SSID = os.environ.get("WIFI_SSID", "HxS-AirLAN")
WIFI_RADIUS_VERIFY_NAME = os.environ.get("WIFI_RADIUS_VERIFY_NAME", "radius.h-x-s.dev")


def access_log_data() -> dict:
    """接続中 (acctstoptime が NULL) のセッションと、最後のセッションを返す。

    AP (hostapd) は端末が切断するときに Stop を送り、AP の再起動時は
    Accounting-Off でその NAS のセッションをすべて閉じるため、
    acctstoptime が NULL ままのセッションが「接続中」を表す。
    時刻は FreeRADIUS が書き込む Unix エポック (整数)。
    """
    # freerad (書き込み側) と並行で動くので読み取り専用で開く
    con = sqlite3.connect(f"file:{DB_PATH}?mode=ro", uri=True, timeout=5)
    try:
        # 同じ部員のセッションが複数開いていても、最初に接続した時刻を出す
        connected = con.execute(
            "SELECT username, MIN(acctstarttime) FROM radacct"
            " WHERE acctstoptime IS NULL"
            " GROUP BY username ORDER BY username"
        ).fetchall()
        last = con.execute(
            "SELECT username, acctstarttime, acctstoptime FROM radacct"
            " ORDER BY acctstarttime DESC LIMIT 1"
        ).fetchone()
    finally:
        con.close()
    return {"connected": connected, "last": last}


def fmt_time(epoch) -> str:
    """radacct の Unix エポックを JST の「月-日 時:分」にする"""
    if epoch is None:
        return "?"
    return datetime.fromtimestamp(int(epoch), tz=JST).strftime("%m-%d %H:%M")


def authentik_list(path: str, params: dict[str, str]) -> list[dict]:
    """Authentik API のページングされた一覧を全部取る"""
    results: list[dict] = []
    page = 1
    while True:
        data = authentik_get(path, {**params, "page": page, "page_size": 100})
        results.extend(data.get("results", []))
        if not data.get("pagination", {}).get("next"):
            return results
        page += 1


def authentik_user_info(usernames: list[str]) -> dict[str, dict]:
    """Authentik username → {"name": 氏名, "discord_id": 連携していれば Discord ID}

    連携していない部員は discord_id が None に入る。Authentik が落ちたときは空。
    """
    if not AUTHENTIK_URL or not AUTHENTIK_API_TOKEN:
        return {}
    pk_to_username: dict[int, str] = {}
    info: dict[str, dict] = {}
    for user in authentik_list("/api/v3/core/users/", {"include_groups": "false"}):
        pk_to_username[user["pk"]] = user["username"]
        info[user["username"]] = {"name": user.get("name") or user["username"], "discord_id": None}
    for connection in authentik_list(
        "/api/v3/sources/user_connections/oauth/",
        {"source__slug": AUTHENTIK_SOURCE_SLUG},
    ):
        username = pk_to_username.get(connection.get("user"))
        if username in info:
            info[username]["discord_id"] = str(connection["identifier"])
    return {username: value for username, value in info.items() if username in usernames}


def authentik_get(path: str, params: dict[str, str] | None = None) -> dict | list:
    """Authentik API に GET する (同期。Bot からは to_thread で呼ぶ)"""
    query = "&".join(f"{key}={urllib.parse.quote(str(value))}" for key, value in (params or {}).items())
    request = urllib.request.Request(
        AUTHENTIK_URL + path + ("?" + query if query else ""),
        headers={"Authorization": f"Bearer {AUTHENTIK_API_TOKEN}"},
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.load(response)


def authentik_username_for_discord(discord_id: str) -> str | None:
    """Discord ID (連携済み) から Authentik の username を返す。連携がなければ None"""
    # search は部分一致のため、identifier との完全一致で絞る
    connections = authentik_get(
        "/api/v3/sources/user_connections/oauth/",
        {"source__slug": AUTHENTIK_SOURCE_SLUG, "search": discord_id},
    )
    for connection in connections.get("results", []):
        if str(connection.get("identifier")) == discord_id:
            user = authentik_get(f"/api/v3/core/users/{connection['user']}/")
            return user["username"]
    return None


def wifi_password(username: str) -> str | None:
    """部員トークン (wifi-<username>) の存在確認。トークンがあれば部員、なければ None

    トークンの key は /wifi の削除後は使わないが、トークンの存在自体が
    /wifi-cert の部員判定 (認可ゲート) として必要なので残す
    """
    try:
        token = authentik_get(f"/api/v3/core/tokens/{WIFI_TOKEN_PREFIX}{username}/view_key/")
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return None
        raise
    return token["key"]


def issue_certificate(username: str, channels: list[str]) -> dict[str, dict]:
    """root helper (wifi-cert-issue) を呼び、チャネルごとの発行結果を返す

    helper は mobileconfig (Apple 端末用プロファイルに埋め込む証明書) と p12
    (それ以外の OS 用) に**別々の証明書**を発行し、失効 (serial 記録) も
    チャネルごとに独立している。channels は発行するチャネル
    (["mobileconfig"], ["p12"], または両方 ["mobileconfig", "p12"])。
    戻り値は発行されたチャネルだけをキーにした {"mobileconfig": {"file", "password", "cn"},
    "p12": {...}}。失敗時 (部員でない・CA がない等) は RuntimeError
    """
    argv = ["sudo", "-n", CERT_HELPER, username]
    if len(channels) == 1:
        argv.append(channels[0])
    result = subprocess.run(
        argv,
        capture_output=True,
        check=False,
        timeout=60,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or f"exit code {result.returncode}")
    # helper はチャネルごとに 1 行の JSON を出す (NDJSON)
    issued: dict[str, dict] = {}
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        item = json.loads(line)
        issued[item.pop("channel")] = item
    if set(issued) != set(channels):
        raise RuntimeError(f"unexpected helper output: {result.stdout!r}")
    return issued


def build_mobileconfig(username: str, p12: bytes, password: str | None) -> bytes:
    """Apple 端末 (iOS / macOS / iPadOS) 用のプロファイル (.mobileconfig) を作る

    p12 (証明書 + 秘密鍵 + CA 証明書) を com.apple.security.pkcs12 payload にそのまま
    入れ、Wi-Fi 設定 (com.apple.wifi.managed) から参照する。これで「インストール」だけで
    証明書の取り込みと Wi-Fi (EAP-TLS) の設定が済む。p12 のパスワードはプロファイルに
    埋め込むため、端末側で入力する場面はない
    """
    # profile identifier / UUID は端末内で一意ならよいので username を入れて分かりやすくする
    cert_uuid = uuid.uuid4().hex.upper()
    profile_uuid = uuid.uuid4().hex.upper()
    cert_payload = {
        "PayloadType": "com.apple.security.pkcs12",
        "PayloadIdentifier": f"dev.h-x-s.wifi.cert.{username}",
        "PayloadUUID": cert_uuid,
        "PayloadVersion": 1,
        "PayloadDisplayName": f"Wi-Fi 証明書 (wifi-{username})",
        "PayloadOrganization": "HxS",
        # bytes は plist の <data> (base64) として書き出される
        "PayloadContent": p12,
        "PayloadCertificateFileName": f"wifi-{username}.p12",
    }
    # p12 が暗号化されていないときは Password を付けない (付けると空文字扱いになり
    # パスワード入力を求められることがある)
    if password:
        cert_payload["Password"] = password
    wifi_payload = {
        "PayloadType": "com.apple.wifi.managed",
        "PayloadIdentifier": f"dev.h-x-s.wifi.network.{username}",
        "PayloadUUID": uuid.uuid4().hex.upper(),
        "PayloadVersion": 1,
        "PayloadDisplayName": f"Wi-Fi ({WIFI_SSID})",
        "PayloadOrganization": "HxS",
        "SSID_STR": WIFI_SSID,
        "HIDDEN_NETWORK": False,
        # WPA / WPA2 / WPA3-Enterprise はすべて "WPA" (Apple の仕様)
        "EncryptionType": "WPA",
        "AutoJoin": True,
        "CaptiveBypass": False,
        # クライアント証明書 (identity) は Wi-Fi payload の PayloadCertificateUUID で
        # pkcs12 payload を参照する (EAPClientConfiguration 内ではない)
        "PayloadCertificateUUID": cert_uuid,
        "EAPClientConfiguration": {
            # 13 = EAP-TLS
            "AcceptEAPTypes": [13],
            # サーバー証明書は OS 標準の信頼ストア (Let's Encrypt) で検証し、
            # 受け付けるサーバー証明書の CN (SAN) をこの名前に限定する
            "TLSTrustedServerNames": [WIFI_RADIUS_VERIFY_NAME],
        },
    }
    profile = {
        "PayloadType": "Configuration",
        "PayloadIdentifier": f"dev.h-x-s.wifi.{username}",
        "PayloadUUID": profile_uuid,
        "PayloadVersion": 1,
        "PayloadDisplayName": f"HxS Wi-Fi (wifi-{username})",
        "PayloadDescription": (
            f"HxS 部室 Wi-Fi ({WIFI_SSID}) のクライアント証明書と接続設定。"
            "期限切れ・再発行のときはこのプロファイルを削除してから新しいものを入れてください"
        ),
        "PayloadOrganization": "HxS",
        "PayloadContent": [cert_payload, wifi_payload],
    }
    return plistlib.dumps(profile)


async def send_connect_prompt(interaction: discord.Interaction) -> None:
    """未連携のときに返す誘導 (設定画面の Connect ボタンと同じ URL)"""
    view = discord.ui.View()
    view.add_item(discord.ui.Button(
        label="Authentik で Discord を連携する",
        url=f"{AUTHENTIK_URL}/source/oauth/login/{AUTHENTIK_SOURCE_SLUG}/",
    ))
    await interaction.followup.send(
        "Authentik に Discord が連携されていません。"
        "次のボタンから連携して、もう一度コマンドを実行してください。",
        view=view,
        ephemeral=True,
    )


class WifiBot(discord.Client):
    def __init__(self) -> None:
        intents = discord.Intents.none()  # スラッシュコマンドのみで intents は不要
        super().__init__(intents=intents)
        self.tree = app_commands.CommandTree(self)

    async def setup_hook(self) -> None:
        # ギルド限定で登録する (グローバル登録は反映に最大1時間かかるため)
        guild = discord.Object(id=GUILD_ID)
        self.tree.copy_global_to(guild=guild)
        await self.tree.sync(guild=guild)


bot = WifiBot()


# 端末種別 → 発行するチャネル。チャネルごとに別の証明書・別の失効 (serial) なので、
# 1 つの端末だけの再発行は他の端末の証明書に影響しない
DEVICE_CHANNELS = {
    "Apple 端末 (iPhone / iPad / Mac)": ["mobileconfig"],
    "Android / Windows など": ["p12"],
    "両方 (全端末で入れ直す)": ["mobileconfig", "p12"],
}


@bot.tree.command(
    name="wifi-cert",
    description="自分の Wi-Fi クライアント証明書を発行して送る (本人にしか見えない)",
)
@app_commands.describe(device="証明書を使う端末。有効期限が切れたときはその端末だけ選べば、他の端末の証明書は無効になりません")
async def wifi_cert(
    interaction: discord.Interaction,
    device: Literal[tuple(DEVICE_CHANNELS)] = "Apple 端末 (iPhone / iPad / Mac)",
) -> None:
    await interaction.response.defer(ephemeral=True)
    if not AUTHENTIK_URL or not AUTHENTIK_API_TOKEN:
        await interaction.followup.send("この Bot は Authentik と連携されていません。管理者に連絡してください。", ephemeral=True)
        return
    try:
        username = await asyncio.to_thread(authentik_username_for_discord, str(interaction.user.id))
    except urllib.error.URLError as error:
        await interaction.followup.send(f"Authentik に接続できませんでした: {error.reason}", ephemeral=True)
        return
    if username is None:
        await send_connect_prompt(interaction)
        return
    try:
        password = await asyncio.to_thread(wifi_password, username)
    except urllib.error.URLError as error:
        await interaction.followup.send(f"Authentik に接続できませんでした: {error.reason}", ephemeral=True)
        return
    if password is None:
        await interaction.followup.send(
            "まだ部員登録が済んでいません (Wi-Fi 用トークンがありません)。"
            "管理者が roles/radius の play を実行するまでお待ちください。",
            ephemeral=True,
        )
        return
    # 発行の認可はここまでの判定 (Discord 連携済み + 部員トークン) で済んでいる。
    # 選ばれた端末種別のチャネルだけを発行する。チャネルごとに別の証明書・別の失効
    # (serial 記録) なので、1 つの端末の再発行は他の端末の証明書に影響しない
    channels = DEVICE_CHANNELS[device]
    try:
        issued = await asyncio.to_thread(issue_certificate, username, channels)
    except (RuntimeError, subprocess.TimeoutExpired) as error:
        await interaction.followup.send(f"証明書を発行できませんでした: {error}", ephemeral=True)
        return
    files = []
    mobileconfig_note = ""
    p12_note = ""
    if "mobileconfig" in issued:
        try:
            with open(issued["mobileconfig"]["file"], "rb") as p12:
                mc_data = p12.read()
        finally:
            os.remove(issued["mobileconfig"]["file"])
        # Apple 端末用は Wi-Fi 設定込みのプロファイル。パスワードもプロファイルに入るので
        # 端末側で入力する必要がない
        mobileconfig = build_mobileconfig(username, mc_data, issued["mobileconfig"].get("password") or None)
        files.append(discord.File(io.BytesIO(mobileconfig), filename=f"wifi-{username}.mobileconfig"))
        mobileconfig_note = (
            "- **iOS / iPadOS / macOS**: `.mobileconfig` を Safari で開いてプロファイルをインストールしてください (パスワード入力不要、Wi-Fi 設定も済みます)。\n"
        )
    if "p12" in issued:
        try:
            with open(issued["p12"]["file"], "rb") as p12:
                p12_data = p12.read()
        finally:
            os.remove(issued["p12"]["file"])
        files.append(discord.File(io.BytesIO(p12_data), filename=f"wifi-{username}.p12"))
        p12_note = (
            f"- **Android / Windows など**: `.p12` をインポートしてください。パスワード: `{issued['p12']['password']}`\n"
            if issued["p12"].get("password")
            else "- **Android / Windows など**: `.p12` をインポートしてください (そのまま使えます、パスワードは不要です)。\n"
        )
    note = (
        f"Wi-Fi クライアント証明書 (CN: `{issued[channels[0]]['cn']}`) を発行しました ({device})。\n"
        "1 年で有効期限が切れます。切れたら**同じ端末を選んで**もう一度 /wifi-cert を実行してください "
        "(端末ごとに証明書が別なので、1 つの端末だけの再発行は他の端末に影響しません)。\n"
    )
    await interaction.followup.send(note + mobileconfig_note + p12_note, files=files, ephemeral=True)


@bot.tree.command(name="access-log", description="Wi-Fi に接続している部員を表示する")
async def access_log(interaction: discord.Interaction) -> None:
    await interaction.response.defer(ephemeral=True)
    data = await asyncio.to_thread(access_log_data)
    # 氏名と連携状況 (username → 氏名/Discord ID) を取る。Authentik が落ちても username だけは出す
    usernames = [row[0] for row in data["connected"]]
    if data["last"] and data["last"][0] not in usernames:
        usernames.append(data["last"][0])
    try:
        info = await asyncio.to_thread(authentik_user_info, usernames)
    except urllib.error.URLError:
        info = {}

    def who(username: str) -> str:
        entry = info.get(username, {})
        name = entry.get("name") or username
        parts = []
        if entry.get("discord_id"):
            parts.append(f"<@{entry['discord_id']}>")
        if name != username:
            parts.append(name)
        parts.append(f"`{username}`")
        if not entry.get("discord_id"):
            parts.append("未連携")
        return " ".join(parts)

    lines = [
        f"- {who(username)} — {fmt_time(started)} から"
        for username, started in data["connected"]
    ]
    if lines:
        text = f"Wi-Fi に接続中 ({len(lines)} 名):\n" + "\n".join(lines)
    else:
        text = "Wi-Fi に接続している部員はいません。"
    last = data["last"]
    if last:
        username, started, stopped = last
        if stopped is None:
            text += f"\n\n最後の利用: {who(username)} — {fmt_time(started)} から接続中"
        else:
            text += f"\n\n最後の利用: {who(username)} — {fmt_time(started)} 〜 {fmt_time(stopped)}"
    # メンションは表示にだけ使い、通知は飛ばさない
    await interaction.followup.send(
        text,
        allowed_mentions=discord.AllowedMentions.none(),
        ephemeral=True,
    )


if __name__ == "__main__":
    bot.run(os.environ["WIFI_DISCORD_BOT_TOKEN"])
