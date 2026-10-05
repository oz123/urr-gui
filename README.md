# urr-gui

Remote **Suspend / Wake** for a pair of boxes, with a small web UI.

Two small statically-linked Go binaries in one module:

| binary  | where it runs   | job                                                            |
|---------|-----------------|---------------------------------------------------------------|
| `urr-gui` | GL.iNet KVM-over-IP box (BusyBox)   | Web UI + state machine. Talks to `mnas` and fires WOL.        |
| `mnas`    | the Gentoo machine to be woken up   | Accepts a token-gated `POST /suspend` and runs `loginctl suspend`. |

Flow: `urr-gui` (arm64) → `mnas` (amd64) on a Gentoo host. `mnas` puts the host to
sleep; `urr-gui` later wakes the host back up by sending a WOL packet.

```
 ┌────────────────────┐   /suspend  ┌────────────────────┐
 │   GL.iNet box      │ ──────────▶ │   Gentoo host       │
 │   (BusyBox, arm64) │             │   (OpenRC, amd64)   │
 │                    │  /status ◀──┤                     │
 │   urr-gui          │             │   mnas ─▶ loginctl  │
 │                    │             │                     │
 │   urr (WOL) ───────┼─ WOL pkt ──▶│   (wakes it back up)│
 └────────────────────┘             └────────────────────┘
```

---

## Repository layout

```
cmd/glkvm/main.go           urr-gui (web UI + state machine)
cmd/glkvm/templates/index.html   urr-gui HTML (embedded via go:embed)
cmd/glkvm/static/milligram.css   Milligram CSS (embedded, served at /css/milligram.css)
cmd/mnas/main.go            mnas (suspend service)
Dockerfile                  cross-compile builder (parameterised by BIN + PKG)
Makefile                    build + install targets
etc/glinet/                 deployables for the GL.iNet box (BusyBox init)
  S99urr-gui                    drop-in for /etc/kvmd/user/scripts/
  urr-gui.env                   sourced config for urr-gui
etc/gentoo/                 deployables for the Gentoo host (OpenRC)
  init.d/mnas-wake-and-suspend  service script (installed as /etc/init.d/mnas-wake-and-suspend)
  mnas-wake-and-suspend         config (installed as /etc/conf.d/mnas-wake-and-suspend)
```

Go `1.26`, no external dependencies.

---

## Building

Docker is the build environment only — the artifacts are raw binaries for the
embedded target (no runtime stage, no container on the device).

```sh
make build          # builds both binaries for arm64 + amd64
make gui            # just urr-gui
make mnas           # just mnas
make clean          # remove out/
```

`make` produces:

```
out/urr-gui-arm64   out/urr-gui-amd64
out/mnas-arm64      out/mnas-amd64
```

> Cross-compilation is done **natively** inside the Go container
> (`CGO_ENABLED=0`, `GOARCH=$GOARCH`). This works without arm64 emulation
> because Go produces statically-linked binaries directly.

---

## Installing

`install*` targets `scp` the binary + the matching init/config to the target
host, then `chmod`. Hosts are overridable (default is a placeholder):

```sh
make install-gui  GUI_HOST=root@192.168.0.72
make install-mnas MNAS_HOST=oznt@gentoo
make install      GUI_HOST=root@192.168.0.72 MNAS_HOST=oznt@gentoo
```

What each target copies (see `Makefile`):

| target         | files                                        | remote locations |
|----------------|----------------------------------------------|------------------|
| `install-gui`  | `out/urr-gui-arm64`                          | `/usr/local/bin/urr-gui` |
|                | `etc/glinet/S99urr-gui`                      | `/etc/kvmd/user/scripts/S99urr-gui` |
|                | `etc/glinet/urr-gui.env`                     | `/etc/kvmd/user/urr-gui.env` |
| `install-mnas` | `out/mnas-amd64`                             | `/usr/local/bin/mnas-wake-and-suspend` |
|                | `etc/gentoo/init.d/mnas-wake-and-suspend`    | `/etc/init.d/mnas-wake-and-suspend` |
|                | `etc/gentoo/mnas-wake-and-suspend`           | `/etc/conf.d/mnas-wake-and-suspend` |

---

## urr-gui

### Endpoints

| method & path   | description |
|-----------------|-------------|
| `GET  /`          | Web UI. Shows current **state** + a **Suspend**/**Wake** button that flips with the state. |
| `GET  /state`     | JSON `{"state","reason","action"}`. |
| `POST/GET /suspend` | Proxies to `mnas` and updates local state. |
| `POST/GET /wake`      | Runs the WOL command (`URR_CMD URR_ARG`) and updates local state. |
| `GET  /css/milligram.css` | Embedded stylesheet. |

### State machine

Three states, held in memory and persisted to `URR_STATE_FILE`:

```
        suspend ok                 wake ok
 unknown ──────────┬──▶ suspended ──────────▶ awake
     ▲             │                         │
     └─────────────┴──── any failure / error ┘
```

- `unknown` — initial, or the last action failed.
- `awake`   — the machine should be up (wake succeeded, or mnas reported awake).
- `suspended` — the machine is asleep (suspend succeeded).

**Boot resolution** (in `main`): ask `mnas` (`GET $MNAS_HOST:$MNAS_PORT/status`)
first — it is the source of truth while it's running; if it's unreachable
(normal when the host is suspended, since it's the sleep target) fall back to
the persisted `URR_STATE_FILE`.

### Environment variables

| var                          | default                  | meaning |
|------------------------------|--------------------------|---------|
| `ADDR`                       | `:8080`                  | listen address |
| `TOKEN`                      | —                        | bearer token sent to `mnas` (must match mnas) |
| `MNAS_HOST`                  | `mnas`                   | host running mnas |
| `MNAS_PORT`                  | `80`                     | mnas port |
| `MNAS_SCHEME`                | `http`                   | `http` or `https` |
| `MNAS_PATH_PREFIX`           | —                        | URL path prefix, e.g. `/wol`, when mnas is reverse-proxied at a sub-path |
| `MNAS_CA_FILE`                | —                        | PEM file to trust mnas's self-signed certificate |
| `MNAS_INSECURE_SKIP_VERIFY`  | `false`                  | skip TLS verification entirely instead of using `MNAS_CA_FILE` |
| `URR_CMD`                    | `urr`                    | WOL binary, resolved via `PATH` |
| `URR_ARG`                    | `mnasx`                  | argument for it — `urr mnasx` sends the WOL packet |
| `URR_STATE_FILE`             | `./state.json`           | where state is persisted |

Requests go to `$MNAS_SCHEME://$MNAS_HOST:$MNAS_PORT$MNAS_PATH_PREFIX/status`
and `.../suspend` — e.g. with `MNAS_SCHEME=https`, `MNAS_PATH_PREFIX=/wol`,
that's `https://192.168.0.150/wol/status`.

`/wake` runs `URR_CMD URR_ARG` (default `urr mnasx`) via
`exec.Command`, which locates the binary from `PATH`, and reports its exit
code / stdout / stderr as JSON.

---

## mnas

Small HTTP service on the target machine. It can only ever answer an HTTP
request while the machine is actually running, so `/status` always reports
`awake` — there's no persisted flag to go stale. (`loginctl suspend` is a
RAM-sleep: this process's memory, including any such flag, would survive
the sleep/resume cycle untouched and keep lying after the machine wakes
back up, which is why there isn't one.)

| method & path  | description |
|----------------|-------------|
| `POST/GET /suspend` | requires `Authorization: Bearer $TOKEN` (constant-time compare). Runs `loginctl suspend` and waits for it: on success returns `202`; if logind rejects it (no privileges, an inhibitor lock, etc.) returns `500` with its stderr/error in the JSON body. |
| `GET /status`   | plain-text `awake`. |
| `GET /health`   | JSON `{"status":"ok",...}`. |

A missing or wrong token returns `401`.

### Environment variables

| var            | default      | meaning |
|----------------|--------------|---------|
| `TOKEN`        | —            | bearer token required on `/suspend` (set this!) |
| `ADDR`         | `:80`        | listen address |
| `SUSPEND_CMD`  | `loginctl`   | command to run on suspend |
| `SUSPEND_ARGS` | `suspend`    | its args (space-separated) |

---

## init / deployment notes

### GL.iNet (BusyBox) — `urr-gui`

`etc/glinet/S99urr-gui` is a POSIX-sh `S99*` hook (same shape as the stock
`S99wireguard`) that the box's `S99custom` runs at boot from
`/etc/kvmd/user/scripts/`. It forks `urr-gui` into the background (never
blocks boot), writes `/var/run/urr-gui.pid`, logs to `/var/log/urr-gui.log`,
and supports `start|stop|restart|status`. Config is read from
`/etc/kvmd/user/urr-gui.env`.

```sh
/etc/kvmd/user/scripts/S99urr-gui start   # e.g. from a shell, or at next boot
```

### Gentoo (OpenRC) — `mnas-wake-and-suspend`

Standard OpenRC service (named `mnas-wake-and-suspend`).

```sh
rc-update add mnas-wake-and-suspend default
rc-service mnas-wake-and-suspend start   # or: rc-service ... restart / status / stop
```

The daemon is installed as `/usr/local/bin/mnas-wake-and-suspend` and the
config lives in `/etc/conf.d/mnas-wake-and-suspend` (`TOKEN`, `ADDR`,
`SUSPEND_CMD`, `SUSPEND_ARGS`). Set a real value for `TOKEN` before first
`start`.

---

## Trying it without real hardware

`urr` (WOL) and `loginctl` can both be faked, so you can exercise the whole
thing locally:

```sh
# 1. fake WOL sender
printf '#!/bin/sh\necho "WOL sent: $@"\n' > urr && chmod +x urr

# 2. fake suspend command
printf '#!/bin/sh\necho "faking: $@"\n' > fake-suspend && chmod +x fake-suspend

# 3. mnas (port 18090) with the fake suspend
TOKEN=demo ADDR=:18090 SUSPEND_CMD=$PWD/fake-suspend ./out/mnas-amd64 &

# 4. urr-gui (port 18083) talking to it
PATH=$PWD:$PATH TOKEN=demo ADDR=:18083 \
  MNAS_HOST=127.0.0.1 MNAS_PORT=18090 \
  URR_STATE_FILE=./state.json ./out/urr-gui-amd64 &

curl -s localhost:18083/state;          echo
curl -s -X POST localhost:18083/suspend; echo
curl -s -X POST localhost:18083/wake;   echo
curl -s localhost:18083/               # the web UI
```

Then open `http://localhost:18083/` in a browser and flip the button.

---

## Security

- Both sides check a shared `TOKEN` (bearer, constant-time compared in `mnas`).
  Set a long random value on **both** hosts (`etc/gentoo/mnas-wake-and-suspend`
  and `etc/glinet/urr-gui.env`).
- Both binaries bind only where you point `ADDR` at — keep them off the public
  interface if they're not meant to be.
- There is no TLS layer in this project; treat these as the two trust
  boundaries and keep both services unreachable from untrusted networks.
