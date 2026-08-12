# Installing

remoses is two static binaries with no dependencies — `remoses`, the daemon,
and `remoses-cli`, a read-only monitor. There is nothing to install alongside
them, no runtime and no libraries, so "installing" means putting two files
somewhere on your PATH. The scripts below do that and check what they
downloaded; doing it by hand is four commands and is documented at the bottom.

## Linux, Raspberry Pi and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/hessu/remoses/main/install.sh | sh
```

| Option | What it does |
|---|---|
| `--version TAG` | Install a particular release instead of the latest. |
| `--prefix DIR` | Install under `DIR/bin`. Default `/usr/local`, which needs `sudo`; `--prefix $HOME/.local` does not. |
| `--systemd` | Also set up a service that starts at boot. See below. |

It picks the build from `uname` and installs `remoses` and `remoses-cli`.

**On a Raspberry Pi, getting this right by hand is easy to get wrong**, which is
most of why the script exists. There are three Pi builds and they are not
interchangeable — an ARMv7 binary on a Pi Zero dies with an illegal instruction.
Worse, Raspberry Pi OS 32-bit has shipped a **64-bit kernel** by default since
Bullseye on the Pi 4, so `uname -m` says `aarch64` on a machine whose userland
is 32-bit and which cannot run an `arm64` binary at all. The script asks
`getconf LONG_BIT`, which reports the userland rather than the kernel, and picks
`armv7` in that case.

### Running it as a service

```sh
curl -fsSL https://raw.githubusercontent.com/hessu/remoses/main/install.sh | sh -s -- --systemd
```

Opt-in, because it is only wanted on a machine that sits by the radio. It:

- creates a system user `remoses`, **in the `dialout` group** — a CAT port is a
  serial device, and the point of a service is that nobody is logged in;
- puts a starting configuration in `/etc/remoses/remoses.yaml`, and **never
  overwrites one that is already there**;
- writes `/etc/systemd/system/remoses.service` with the usual hardening;
- and stops. **The service is not started**, because the configuration it just
  wrote still has the example passwords in it.

When you have edited the configuration:

```sh
sudo systemctl enable --now remoses
systemctl status remoses
journalctl -u remoses -f
```

## Windows

```powershell
irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1 | iex
```

`irm` and `iex` are built into Windows PowerShell 5.1, which ships with Windows
10 and 11 — there is nothing to install first, and PowerShell 7 is not needed.

It installs per-user into `%LOCALAPPDATA%\Programs\remoses` and adds that to
your PATH, so **no administrator rights are required**. Open a new terminal
afterwards for the PATH to take effect.

To pass options, a piped script has nowhere to put arguments, so:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1))) -Version v0.1.0
```

`-Dir` chooses somewhere else, `-NoPath` leaves your PATH alone.

On Windows a radio is usually a COM port: put that in `port.device`, for
example `COM7`. Device Manager lists them under **Ports (COM & LPT)**.

### Why reading the script first is the fiddlier path

```powershell
irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1 -OutFile install.ps1
notepad install.ps1
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

That last line needs `-ExecutionPolicy Bypass` while the one-liner does not,
which is exactly backwards from how it ought to feel. Execution policy governs
script *files*, not commands piped into a session, and Microsoft's own
documentation is blunt about it: it "isn't a security system that restricts user
actions… users can easily bypass a policy by typing the script contents at the
command line".

So the execution policy is not what is protecting you either way. **The checksum
is**, and it runs in both forms.

## What the scripts check

Both download the release's `SHA256SUMS`, find the line for the archive they
just fetched, and compare. A mismatch stops the install **before anything is
unpacked** — a checksum verified after the fact is decoration.

If you would rather not run a script off the internet at all, that is a
perfectly reasonable position, and the manual route is below.

## By hand

Pick the archive for your machine from the
[releases](https://github.com/hessu/remoses/releases) — see the table in the
[README](../README.md#downloads) for which — then:

```sh
curl -fsSLO https://github.com/hessu/remoses/releases/download/v0.1.0/remoses-v0.1.0-linux-arm64.tar.gz
curl -fsSLO https://github.com/hessu/remoses/releases/download/v0.1.0/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
tar xzf remoses-v0.1.0-linux-arm64.tar.gz
sudo cp remoses-v0.1.0-linux-arm64/remoses* /usr/local/bin/
```

On Windows, unpack the `.zip` and put the two `.exe` files wherever you keep
such things; `Get-FileHash -Algorithm SHA256` checks the download against
`SHA256SUMS`.

## Then what

```sh
remoses -version
remoses passwd                        # a password hash for the configuration
remoses -config remoses.yaml -check   # validate before opening a port
remoses test-run                      # exercise your radio, write a report
```

Copy `remoses.example.yaml` — it ships in the archive — and edit it. It is
annotated throughout, and [configuration.md](configuration.md) covers the same
ground in prose. Then find your radio's page: [Icom](icom.md),
[Kenwood](kenwood.md), [Yaesu](yaesu.md) or [rigctld](rigctld.md).

If your radio is one of the many that has never been tested against real
hardware, **[test-run.md](test-run.md)** is the most useful thing you can do
with it.

## Upgrading and removing

Rerun the installer; it overwrites the binaries and leaves your configuration
alone. With a service, restart it afterwards: `sudo systemctl restart remoses`.

To remove: delete the two binaries, and on a service install also
`/etc/systemd/system/remoses.service` (after `sudo systemctl disable --now
remoses`), `/etc/remoses`, and the `remoses` user.
