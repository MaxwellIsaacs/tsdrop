<p align="center"><img src="assets/tsdrop.svg" width="96" alt="tsdrop icon"></p>

# tsdrop

AirDrop for your tailnet: send files and links between your Linux machines and
your iPhone over [Tailscale](https://tailscale.com), using the
[LocalSend](https://localsend.org) protocol.

- **Linux → anything:** `tsdrop send --to phone photo.jpg`, or a small GTK picker window.
- **iPhone → Linux:** a share-sheet Shortcut or a home-screen web app. No app to install.
- **Linux → iPhone without an app:** files wait in an outbox. You get a real iOS
  notification, and tapping it opens the item.
- **Laptop asleep?** An always-on box (your homelab) holds drops and forwards
  them when the laptop comes back.
- **Text and links** go straight to the clipboard, with a desktop notification.

Everything stays inside your tailnet. Every listener refuses non-Tailscale addresses.

## Why not Taildrop?

[Taildrop](https://tailscale.com/kb/1106/taildrop) is built in and covers basic
file sending. tsdrop fills the gaps that bothered me:

| | Taildrop | tsdrop |
|---|---|---|
| Laptop asleep when you send | ❌ fails | ✅ the relay holds it, then forwards |
| Text / links | a `.txt` file | ✅ clipboard + notification |
| Linux receive | `tailscale file get` loop, no notification | ✅ `~/Downloads` + notification |
| iPhone receive | files sit in the Tailscale app, no notification | ✅ push notification → opens the item |
| Talks to the LocalSend app | ❌ | ✅ |

If you have a homelab box that's always on, tsdrop is the more convenient choice.

## How it fits together

```
 iPhone ── share sheet / web app ──▶  relay (homelab, always on) ──LocalSend──▶  laptop
                                        │  spool: holds drops while laptop sleeps
 laptop ── tsdrop send --to iphone ──▶  │  outbox: holds items for the phone
                                        └─ Web Push ──▶ iPhone notification ──▶ web app
```

- `tsdrop recv` is a LocalSend-compatible receiver on `:53317` plus a small web app on `:53318`.
- `tsdrop recv --forward HOST` turns it into a relay that stores and forwards everything to `HOST`.
- `tsdrop send` talks LocalSend to any peer that runs it. An iPhone without LocalSend
  open gets the item through the outbox instead.

## Install

Needs Go 1.23+ and Tailscale. The GUI also needs Python 3 with PyGObject and GTK 4.

```sh
go install github.com/MaxwellIsaacs/tsdrop@latest   # just the binary, or:
make install                               # binary, desktop entry, icon, user unit
systemctl --user enable --now tsdrop-recv  # receive into ~/Downloads
```

### The relay (optional, recommended)

On the always-on box:

```sh
make install-relay     # static binary + tsdrop-relay.service
# edit FORWARD (your laptop's tailnet name) and HTTPS_URL in the unit, then:
systemctl --user daemon-reload && systemctl --user enable --now tsdrop-recv
```

On your laptop, point `send` at the relay's outbox:

```sh
echo http://RELAY-IP:53318 > ~/.config/tsdrop/outbox
```

### iPhone notifications

Web Push needs HTTPS. Tailscale provides it:

```sh
sudo tailscale serve --bg --https=53319 http://127.0.0.1:53318
# then set HTTPS_URL=https://RELAY.your-tailnet.ts.net:53319 in the relay unit
```

On the phone (iOS 16.4+):

1. Open the HTTPS address in Safari, then **Share → Add to Home Screen**.
2. Open the new icon and tap **turn on notifications**.

### iPhone share sheet

**[Get the tsdrop Shortcut](https://www.icloud.com/shortcuts/b904536ab5fb4e9fac437617a410812c)**, or build
one that accepts *Images, Media, Files, Text, URLs* from the share sheet:

- **Repeat with Each** item → **Get Contents of URL**
  `POST http://RELAY-IP:53318/drop?from=iPhone`
  - files: Request Body **Form**, field `file` (type File) = Repeat Item
  - text/links: Request Body **File** = Repeat Item (arrives on the laptop's clipboard)

## Usage

```
tsdrop peers [--all] [--json]          list tailnet peers; ● LocalSend, ◐ via outbox
tsdrop send [--to PEER] FILE|DIR...    send files
tsdrop send [--to PEER] --text "msg"   send text / a link
tsdrop gui                             picker window (bind it to a key)
tsdrop recv [--dir DIR]                receive
tsdrop recv --forward PEER [--dir SPOOL] [--https-url URL]   relay
```

## HTTP API (web app port, tailnet only)

| | |
|---|---|
| `POST /drop` | multipart files, raw body with `?name=`, or text → the receiver |
| `GET /outbox` | items waiting for the phone (JSON) |
| `POST /outbox?name=F` | queue a file (raw body); no `name` = text |
| `GET` / `DELETE /outbox/{id}` | fetch / dismiss an item |
| `GET /push/key`, `POST /push/subscribe` | Web Push (VAPID) |

## License

MIT
