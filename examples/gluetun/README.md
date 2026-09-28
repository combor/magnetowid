# Gluetun VPN examples

Keep magnetowid installed using the [package for your platform](../../docs/usage.md#install-and-run).
On Linux, use the systemd service supplied by the `.deb`, `.rpm`, or AUR package.
Gluetun runs in a container and provides the VPN connection.

Choose one setup:

- [HTTP proxy](#http-proxy): configure magnetowid and FFmpeg to use Gluetun's
  proxy. Check [FFmpeg compatibility](#ffmpeg-compatibility) first.
- [Linux network namespace](#linux-network-namespace): route the packaged
  service's networking and DNS through Gluetun, including FFmpeg.

For TVP VOD, choose a VPN endpoint in Poland. These examples use your provider's
WireGuard configuration, saved as `wg0.conf`. Restrict access to that file to
your account or the service administrator. See
[Gluetun's WireGuard options](https://github.com/qdm12/gluetun-wiki/blob/main/setup/options/wireguard.md)
for additional VPN settings. If you already use a VPN client, you can use its
system tunnel or application routing instead, including magnetowid, FFmpeg,
and DNS.

## HTTP proxy

### FFmpeg compatibility

An affected Gluetun/FFmpeg combination cannot download HTTPS streams through
the HTTP proxy, even when catalogue searches and Arr connection tests pass.
Gluetun can send `Transfer-Encoding: chunked` in a successful HTTP CONNECT
response. Affected FFmpeg builds interpret the following TLS bytes as HTTP
chunks and close the connection, reporting `Last chunk received` followed by
`Error in the pull function` or an I/O error.

The [CONNECT specification](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.3.6)
requires the proxy to omit this header and the client to ignore it if present.
This failure happens during tunnel setup; disabling certificate verification
does not fix it. A successful catalogue search does not establish FFmpeg
compatibility. Verify a complete download with your installed versions before
relying on this route. The [Privoxy workaround](#native-workaround-with-privoxy)
keeps magnetowid and FFmpeg native while using Gluetun's HTTP proxy. Your VPN
client's system tunnel and the [Linux network namespace setup](#linux-network-namespace)
are also options.

### Configure Gluetun

If you already run Gluetun, enable its
[HTTP proxy](https://github.com/qdm12/gluetun-wiki/blob/main/setup/options/http-proxy.md)
and publish its proxy port to the host running magnetowid.

For a new Gluetun container, install
[Docker with Compose](https://docs.docker.com/compose/install/).
Download [compose.proxy.yaml](compose.proxy.yaml) into a working directory
alongside `wg0.conf`. Edit it for your VPN, then run:

```sh
docker compose -p magnetowid-proxy -f compose.proxy.yaml up -d --wait
```

This publishes Gluetun's proxy at `http://127.0.0.1:8888`. Magnetowid keeps
running as a native application. If Gluetun runs on another machine, use an
address reachable from magnetowid and restrict proxy access to your network.

### Native workaround with Privoxy

Install [Privoxy for your platform](https://www.privoxy.org/user-manual/installation.html).
The connection path becomes `magnetowid / FFmpeg -> Privoxy -> Gluetun -> TVP`.
Privoxy's [downgrade-http-version action](https://www.privoxy.org/user-manual/actions-file.html#DOWNGRADE-HTTP-VERSION)
makes the CONNECT exchange use HTTP/1.0, avoiding Gluetun's chunked response.
HTTPS traffic inside the tunnel stays encrypted and uses its original protocol.

Download [privoxy.conf](privoxy.conf) and [gluetun.action](gluetun.action).
Edit `confdir` and `logdir` for your Privoxy installation, and set the `forward`
address to Gluetun's published proxy port. Use this configuration for a Privoxy
instance dedicated to magnetowid. It forwards every destination through Gluetun;
keep the catch-all forwarding rule and avoid direct-connection exceptions.

For Linux packages using `/etc/privoxy/config` and `privoxy.service`:

```sh
sudo install -m 644 privoxy.conf /etc/privoxy/config
sudo install -m 644 gluetun.action /etc/privoxy/gluetun.action
sudo privoxy --config-test /etc/privoxy/config
sudo systemctl enable privoxy
sudo systemctl restart privoxy
```

On other platforms, select this configuration in Privoxy's service settings,
or start it with `privoxy --no-daemon /path/to/privoxy.conf` (`privoxy.exe` on
Windows). In the magnetowid settings below, use `http://127.0.0.1:8118` for all
four proxy URLs. The additional native proxy process is the only extra runtime
component; magnetowid and FFmpeg use their installed packages.

### Configure magnetowid

Download [proxy.env](proxy.env) and edit the proxy address. Both magnetowid's
HTTP client and FFmpeg need the settings. Keep lowercase `http_proxy` set:
FFmpeg uses it for HTTPS as well as HTTP. The `NO_PROXY` and `no_proxy` settings
keep local requests direct.

For a packaged Linux service, download
[magnetowid-privoxy.conf](magnetowid-privoxy.conf) when using the workaround, or
[magnetowid-proxy.conf](magnetowid-proxy.conf) for a direct Gluetun connection.
Save the selected file as `proxy.conf` and install the edited files:

```sh
sudo install -m 600 proxy.env /etc/magnetowid/proxy.env
sudo install -Dm 644 proxy.conf /etc/systemd/system/magnetowid.service.d/proxy.conf
sudo systemctl daemon-reload
sudo systemctl restart magnetowid
```

For Homebrew, Scoop, Nix, or archive installations, add the variables from
`proxy.env` to the environment used to launch magnetowid, then restart it.
FFmpeg inherits that environment. A shell's environment does not automatically
apply to an existing background service.

When switching from the namespace example, stop magnetowid and run
`sudo systemctl disable --now magnetowid-gluetun`. Remove magnetowid's
`vpn.conf` override, run `sudo systemctl daemon-reload`, and follow the proxy
Compose setup above. Stopping the old container releases its published API
port for the native magnetowid service.
Set `MAGNETOWID_LISTEN=127.0.0.1:8484` if both Arr apps run on the same host.

Sonarr and Radarr connect to magnetowid's usual API address and use its usual
download directory. Follow [Connect and verify](#connect-and-verify), including
the complete download test.

## Linux network namespace

Install a [Linux service package](../../docs/usage.md#linux-service) for your
distribution: `.deb`, `.rpm`, or `magnetowid-bin` from the AUR. Install
[Docker Engine](https://docs.docker.com/engine/install/) and its Compose plugin,
then enable Docker with `sudo systemctl enable --now docker`.

This setup runs the native magnetowid service with Gluetun's networking and
DNS. Downloads stay on the host, and the API is published on host loopback.
The systemd files in this section apply only to Linux.

Download the example files into a working directory:

```sh
mkdir magnetowid-vpn
cd magnetowid-vpn
for file in compose.yaml magnetowid-gluetun.service magnetowid-vpn.conf netns.sh resolv.conf; do
  curl -fSLO "https://raw.githubusercontent.com/combor/magnetowid/main/examples/gluetun/$file"
done
```

Edit [compose.yaml](compose.yaml) for your VPN.
It uses a WireGuard configuration supplied by your VPN provider.
Keep the firewall enabled and the published port bound to loopback.

The [systemd service](magnetowid-gluetun.service),
[magnetowid override](magnetowid-vpn.conf), and
[namespace helper](netns.sh) use matching default
names and paths. Edit them together if you change those defaults.

### Install the service files

Replace `/path/to/wireguard.conf` with your provider's configuration file:

```sh
sudo install -d -m 700 /etc/magnetowid-gluetun
sudo install -m 600 /path/to/wireguard.conf /etc/magnetowid-gluetun/wg0.conf
sudo install -m 644 compose.yaml resolv.conf /etc/magnetowid-gluetun/
sudo install -m 700 netns.sh /etc/magnetowid-gluetun/
sudo install -m 644 magnetowid-gluetun.service /etc/systemd/system/
sudo install -Dm 644 magnetowid-vpn.conf /etc/systemd/system/magnetowid.service.d/vpn.conf
sed 's/^hosts:.*/hosts: files dns/' /etc/nsswitch.conf | sudo tee /etc/magnetowid-gluetun/nsswitch.conf >/dev/null
```

The resolver files keep DNS lookups inside Gluetun, including FFmpeg lookups
on hosts that use systemd-resolved.

Edit `/etc/magnetowid/magnetowid.env`:

- Set `MAGNETOWID_API_KEY` to a long random key, for example one generated by
  `openssl rand -hex 32`. Keep the environment file mode `600`.
- Set `MAGNETOWID_LISTEN=:8484`. Docker publishes this port only on host loopback.
- Use the default download directory, or set `MAGNETOWID_DOWNLOAD_DIR` to a
  folder writable by magnetowid and both Arr services. See
  [Linux service](../../docs/usage.md#linux-service) for permissions.
- Remove HTTP and HTTPS proxy settings from the service; Gluetun routes its
  network traffic directly through the VPN. If you installed the proxy example,
  remove `/etc/systemd/system/magnetowid.service.d/proxy.conf` before continuing.

Start both services and enable them at boot:

```sh
sudo systemctl daemon-reload
sudo systemctl stop magnetowid
sudo systemctl enable --now magnetowid-gluetun magnetowid
curl --fail http://127.0.0.1:8484/health
```

For this setup, restart Gluetun with `sudo systemctl restart magnetowid-gluetun`
so magnetowid rejoins its network namespace. Restarting only the container can
leave magnetowid in the old namespace.

## Connect and verify

Follow [Sonarr / Radarr setup](../../docs/usage.md#sonarr--radarr-setup) using
the address reachable from your apps and the same magnetowid API key for the
download client and indexer. Select magnetowid as the indexer's download client.
Enable [subtitle import](../../docs/usage.md#subtitles) if wanted.

Test both connections, then search for a title and download it. Check that
Sonarr or Radarr imports the completed video and any enabled subtitles;
connection tests alone do not exercise downloads or library imports.
