# Region-locked sites

Sites limit their streams to their own country:

| Site | Country | Setting |
|---|---|---|
| TVP VOD | Poland | `MAGNETOWID_TVP_PROXY` |
| BBC iPlayer | United Kingdom | `MAGNETOWID_BBC_PROXY` |

A site in the country magnetowid runs in needs nothing. For any other site,
run a **VPN exit** in the site's country and name it in the site's setting.
magnetowid then reaches that site, and only that site, through the exit, so
one install can use sites from different countries.

An exit is a [Gluetun](https://github.com/qdm12/gluetun) container connected
to your VPN provider. You need Docker and a VPN subscription with servers in
the countries you want.

- [Linux service](#linux-service)
- [Docker Compose](#docker-compose)
- [The site settings](#the-site-settings)
- [Troubleshooting](#troubleshooting)

## Linux service

The packages include `magnetowid-vpn@.service`, which runs one exit per name.
The name is yours to choose; these steps use `pl` for an exit in Poland.

### One site

**1. Describe the exit.** Copy the example and edit the copy:

```sh
sudo install -m 600 /usr/share/magnetowid/vpn.env.example /etc/magnetowid/vpn-pl.env
sudoedit /etc/magnetowid/vpn-pl.env
```

[The example](../vpn.env.example) looks like this:

```sh
# The country this exit must be in.
SERVER_COUNTRIES=Poland

# Your VPN provider and its credentials. Gluetun's wiki lists the lines each
# provider needs: https://github.com/qdm12/gluetun-wiki/tree/main/setup/providers
VPN_SERVICE_PROVIDER=
VPN_TYPE=

# magnetowid reaches the exit through Gluetun's HTTP proxy.
HTTPPROXY=on

# Linux service only: the local port of this exit's proxy. Give each exit its own.
MAGNETOWID_VPN_PORT=8888
```

Fill in `VPN_SERVICE_PROVIDER` and `VPN_TYPE`, and add the credential lines
[your provider's page](https://github.com/qdm12/gluetun-wiki/tree/main/setup/providers)
lists. Leave the other lines as they are.

**2. Start the exit**, now and at every boot:

```sh
sudo systemctl enable --now magnetowid-vpn@pl
```

**3. Check its country.** The first start downloads Gluetun, so allow a minute:

```sh
curl -x http://127.0.0.1:8888 https://ipinfo.io/country
```

This must print `PL`. If it doesn't, see [Troubleshooting](#troubleshooting).

**4. Send the site through it.** Add to `/etc/magnetowid/magnetowid.env`:

```sh
MAGNETOWID_TVP_PROXY=http://127.0.0.1:8888
```

```sh
sudo systemctl restart magnetowid
```

### A second site

Repeat the steps with another name, country and port. For BBC iPlayer:

```sh
sudo install -m 600 /usr/share/magnetowid/vpn.env.example /etc/magnetowid/vpn-gb.env
sudoedit /etc/magnetowid/vpn-gb.env
```

In `vpn-gb.env`, fill in the provider lines as before and change two lines:

```sh
SERVER_COUNTRIES=United Kingdom
MAGNETOWID_VPN_PORT=8889
```

Your provider may want separate credentials, such as a second WireGuard key,
for a second connection at the same time.

```sh
sudo systemctl enable --now magnetowid-vpn@gb
curl -x http://127.0.0.1:8889 https://ipinfo.io/country
```

This must print `GB`. `/etc/magnetowid/magnetowid.env` then names both exits:

```sh
MAGNETOWID_TVP_PROXY=http://127.0.0.1:8888
MAGNETOWID_BBC_PROXY=http://127.0.0.1:8889
```

```sh
sudo systemctl restart magnetowid
```

## Docker Compose

[`compose.vpn.yaml`](../compose.vpn.yaml) adds an exit in Poland, `vpn-pl`, to
the [quick start](../README.md#quick-start)'s `compose.yaml`.

### One site

**1. Get the files**, in the folder that holds `compose.yaml`:

```sh
curl -fsSLO https://raw.githubusercontent.com/combor/magnetowid/main/compose.vpn.yaml
curl -fsSL https://raw.githubusercontent.com/combor/magnetowid/main/vpn.env.example -o vpn-pl.env
chmod 600 vpn-pl.env
```

**2. Describe the exit.** In `vpn-pl.env`, fill in `VPN_SERVICE_PROVIDER` and
`VPN_TYPE`, and add the credential lines
[your provider's page](https://github.com/qdm12/gluetun-wiki/tree/main/setup/providers)
lists.

**3. Use both Compose files** from now on. Add to `.env`:

```sh
COMPOSE_FILE=compose.yaml:compose.vpn.yaml
```

**4. Start, and check the exit's country:**

```sh
docker compose up -d
docker compose exec vpn-pl wget -qO- https://ipinfo.io/country
```

This must print `PL`. TVP VOD already goes through the exit:
`compose.vpn.yaml` sets `MAGNETOWID_TVP_PROXY` to `http://vpn-pl:8888`.

### A second site

Copy `vpn-pl.env` to `vpn-gb.env`, set `SERVER_COUNTRIES=United Kingdom` in
the copy, and give it separate credentials if your provider wants them for a
second connection. Then add the exit and the site's setting to
`compose.vpn.yaml`:

```yaml
services:
  magnetowid:
    environment:
      MAGNETOWID_TVP_PROXY: http://vpn-pl:8888
      MAGNETOWID_BBC_PROXY: http://vpn-gb:8888
    depends_on:
      - vpn-pl
      - vpn-gb

  vpn-pl:
    image: qmcgaw/gluetun:v3
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    env_file: vpn-pl.env

  vpn-gb:
    image: qmcgaw/gluetun:v3
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
    env_file: vpn-gb.env
```

```sh
docker compose up -d
docker compose exec vpn-gb wget -qO- https://ipinfo.io/country
```

This must print `GB`.

## The site settings

Each takes one of:

| Value | The site is reached |
|---|---|
| `http://host:port` | through that HTTP proxy |
| `direct` | without a proxy, even if `HTTPS_PROXY` is set |
| not set | as before: through `HTTPS_PROXY` if it is set, otherwise directly |

The command-line flags are `-tvp-proxy` and `-bbc-proxy`. A site's searches,
streams and subtitles all use its setting. magnetowid logs every site's setting
when it starts.

While an exit is down, its site's downloads wait as `provider unreachable`
and resume by themselves. They are never sent without the exit.

## Troubleshooting

| Problem | What to check |
|---|---|
| The exit doesn't start | `systemctl status magnetowid-vpn@pl` and `journalctl -u magnetowid-vpn@pl -n 50`, or `docker compose logs vpn-pl`. A status of `78/CONFIG` means `VPN_SERVICE_PROVIDER` or `MAGNETOWID_VPN_PORT` is missing from the exit's file. Other errors come from Gluetun and name the setting it rejects: a missing credential, or a country your provider's Gluetun page spells differently. |
| Gluetun rejects the country although it is right | `VPN_SERVICE_PROVIDER` is blank, so Gluetun assumed a provider of its own. |
| The country check fails to connect | The exit is still starting, or the port isn't the exit's `MAGNETOWID_VPN_PORT`. |
| One site's results are empty | Its streams are refused: the log's search lines show `unavailable` above zero. Check that site's setting and its exit's country, then restart magnetowid. |
| Downloads wait as `provider unreachable` | The site's exit is down. Start it; the downloads resume. |
| A download fails with an ffmpeg `Input/output error` | magnetowid fetches streams itself where it can; live and encrypted ones are left to ffmpeg, which can't connect through Gluetun's proxy. These fail instead of bypassing the exit. |
