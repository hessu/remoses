#!/bin/sh
# remoses installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/hessu/remoses/main/install.sh | sh
#
# It works out which build your machine needs, downloads it from the GitHub
# release, checks it against the published SHA256SUMS, and copies two binaries
# into place. Nothing else is installed and nothing is left running.
#
# To read it before running it — which is the right instinct:
#
#   curl -fsSLO https://raw.githubusercontent.com/hessu/remoses/main/install.sh
#   less install.sh && sh install.sh
#
# POSIX sh on purpose: this ends up on Raspberry Pi OS, on a shack PC, and
# occasionally on something older than either.

set -eu

REPO="${REMOSES_REPO:-hessu/remoses}"
BASE_URL="${REMOSES_BASE_URL:-https://github.com/$REPO/releases}"
API_URL="${REMOSES_API_URL:-https://api.github.com/repos/$REPO/releases/latest}"

PREFIX="${PREFIX:-/usr/local}"
VERSION=""
WANT_SYSTEMD=0
SUDO=""

usage() {
	cat <<EOF
remoses installer

usage: install.sh [options]

  -v, --version TAG   install this release (default: the latest)
  -p, --prefix DIR    install under DIR/bin (default: $PREFIX)
      --systemd       also create a system service that starts at boot
  -h, --help          this

The service is opt-in because it is only wanted on a machine that sits by the
radio. It creates a "remoses" user in the dialout group, puts a configuration
in /etc/remoses, and enables a unit — and it never overwrites a configuration
that is already there.
EOF
}

die() {
	echo "install.sh: $*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
	case "$1" in
	-v | --version)
		VERSION="${2:-}"
		[ -n "$VERSION" ] || die "--version needs a tag"
		shift 2
		;;
	-p | --prefix)
		PREFIX="${2:-}"
		[ -n "$PREFIX" ] || die "--prefix needs a directory"
		shift 2
		;;
	--systemd)
		WANT_SYSTEMD=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
done

# --- which build ------------------------------------------------------------

# detect_platform sets PLATFORM to the label used in the release file names.
#
# The 32-bit ARM cases are the reason this script exists. A Raspberry Pi Zero is
# ARMv6 and a Pi 2 is ARMv7, and the wrong one of those does not fail politely:
# it dies with an illegal instruction, which is a horrible thing to work out
# over a slow link to a remote site.
#
# The trap underneath it is worse. Raspberry Pi OS 32-bit has shipped a 64-bit
# *kernel* by default since Bullseye on the Pi 4, so `uname -m` reports aarch64
# on a machine whose userland is 32-bit armhf and which cannot run an arm64
# binary at all. `getconf LONG_BIT` reports the userland rather than the kernel,
# so it is what actually answers the question.
detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)

	case "$os" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported operating system: $os (releases cover Linux, macOS and Windows)" ;;
	esac

	bits=64
	if have getconf; then
		bits=$(getconf LONG_BIT 2>/dev/null || echo 64)
	fi

	case "$arch" in
	x86_64 | amd64)
		arch=amd64
		;;
	aarch64 | arm64)
		if [ "$bits" = "32" ]; then
			# 64-bit kernel, 32-bit userland: a Raspberry Pi running the
			# 32-bit OS. Debian calls this armhf, and armhf is ARMv7.
			arch=armv7
		else
			arch=arm64
		fi
		;;
	armv7* | armv8l)
		arch=armv7
		;;
	armv6*)
		arch=armv6
		;;
	arm*)
		die "unrecognised ARM variant '$arch'. The releases cover armv6 (Pi 1, Zero),
armv7 (Pi 2 and later on a 32-bit OS) and arm64 (Pi 3 and later on a 64-bit OS);
pick one by hand from $BASE_URL"
		;;
	*)
		die "no release build for $os/$arch — see $BASE_URL"
		;;
	esac

	if [ "$os" = darwin ] && [ "$arch" != amd64 ] && [ "$arch" != arm64 ]; then
		die "no macOS build for $arch"
	fi
	PLATFORM="$os-$arch"
}

latest_version() {
	# The GitHub API without a token, which is rate limited per address but
	# ample for an install. grep and sed rather than a JSON parser, because
	# requiring jq to install a program with no dependencies would be a poor
	# joke.
	fetch "$API_URL" |
		grep -m1 '"tag_name"' |
		sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//'
}

fetch() {
	if have curl; then
		curl -fsSL "$1"
	elif have wget; then
		wget -qO- "$1"
	else
		die "neither curl nor wget is installed"
	fi
}

fetch_to() {
	if have curl; then
		curl -fsSL -o "$2" "$1"
	elif have wget; then
		wget -qO "$2" "$1"
	else
		die "neither curl nor wget is installed"
	fi
}

# sha256_of prints the checksum of a file, using whichever tool this system has.
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | cut -d' ' -f1
	elif have shasum; then
		shasum -a 256 "$1" | cut -d' ' -f1
	elif have openssl; then
		openssl dgst -sha256 "$1" | sed 's/.*= *//'
	else
		echo ""
	fi
}

# --- do it ------------------------------------------------------------------

detect_platform

if [ -z "$VERSION" ]; then
	echo "looking up the latest release..."
	VERSION=$(latest_version)
	[ -n "$VERSION" ] || die "could not work out the latest release; pass --version"
fi

ARCHIVE="remoses-$VERSION-$PLATFORM.tar.gz"
URL="$BASE_URL/download/$VERSION/$ARCHIVE"
SUMS_URL="$BASE_URL/download/$VERSION/SHA256SUMS"

echo "remoses $VERSION for $PLATFORM"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/remoses.XXXXXX")
# shellcheck disable=SC2064  # $TMP is wanted now, not at trap time
trap "rm -rf '$TMP'" EXIT INT TERM

echo "downloading $ARCHIVE"
fetch_to "$URL" "$TMP/$ARCHIVE" ||
	die "could not download $URL
if that release has no build for $PLATFORM, the list is at $BASE_URL"

# Verify before unpacking. A checksum checked after the fact is decoration.
echo "checking the checksum"
if fetch_to "$SUMS_URL" "$TMP/SHA256SUMS" 2>/dev/null; then
	want=$(grep " \*\{0,1\}$ARCHIVE\$" "$TMP/SHA256SUMS" | cut -d' ' -f1 || true)
	got=$(sha256_of "$TMP/$ARCHIVE")
	if [ -z "$want" ]; then
		die "SHA256SUMS does not list $ARCHIVE"
	elif [ -z "$got" ]; then
		echo "  no sha256 tool here, so the checksum could not be verified" >&2
	elif [ "$want" != "$got" ]; then
		die "CHECKSUM MISMATCH for $ARCHIVE
  expected $want
  got      $got
Do not use this download."
	else
		echo "  ok"
	fi
else
	die "could not fetch SHA256SUMS from $SUMS_URL"
fi

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" || die "could not unpack $ARCHIVE"
SRC="$TMP/remoses-$VERSION-$PLATFORM"
[ -x "$SRC/remoses" ] || die "the archive does not contain a remoses binary"

BINDIR="$PREFIX/bin"

# Whether this needs sudo is a question about the nearest directory that
# actually exists, not about the target. --prefix $HOME/.local usually names
# something not there yet, and asking for a password to create a directory
# inside your own home would be daft — which is exactly what an earlier version
# of this did, on the very prefix the error message above recommends.
probe="$BINDIR"
while [ ! -e "$probe" ]; do
	parent=$(dirname "$probe")
	[ "$parent" != "$probe" ] || break
	probe="$parent"
done
if [ ! -w "$probe" ] && [ "$(id -u)" != "0" ]; then
	have sudo || die "$BINDIR is not writable and sudo is not installed.
Install somewhere you own instead:  install.sh --prefix \$HOME/.local"
	SUDO="sudo"
	echo "installing to $BINDIR needs sudo"
fi

$SUDO mkdir -p "$BINDIR"
$SUDO cp "$SRC/remoses" "$SRC/remoses-cli" "$BINDIR/"
$SUDO chmod 0755 "$BINDIR/remoses" "$BINDIR/remoses-cli"
echo "installed $BINDIR/remoses and $BINDIR/remoses-cli"

# --- the service, if asked for ----------------------------------------------

setup_systemd() {
	have systemctl || die "--systemd was given but systemctl is not here"
	[ "$(uname -s)" = Linux ] || die "--systemd is for Linux"
	if [ "$(id -u)" != "0" ]; then
		have sudo || die "--systemd needs root"
		SUDO="sudo"
	fi

	# A user of its own, in dialout: a CAT port is a serial device, and the
	# whole reason to run this as a service is that nobody is logged in.
	if ! id remoses >/dev/null 2>&1; then
		echo "creating the remoses user"
		$SUDO useradd --system --no-create-home --shell /usr/sbin/nologin \
			--groups dialout remoses ||
			die "could not create the remoses user"
	else
		$SUDO usermod -a -G dialout remoses || true
	fi

	$SUDO mkdir -p /etc/remoses
	if [ -f /etc/remoses/remoses.yaml ]; then
		echo "keeping the configuration already in /etc/remoses/remoses.yaml"
	else
		$SUDO cp "$SRC/remoses.example.yaml" /etc/remoses/remoses.yaml
		$SUDO chown root:remoses /etc/remoses/remoses.yaml
		$SUDO chmod 0640 /etc/remoses/remoses.yaml
		echo "wrote a starting configuration to /etc/remoses/remoses.yaml"
	fi

	echo "writing /etc/systemd/system/remoses.service"
	$SUDO tee /etc/systemd/system/remoses.service >/dev/null <<EOF
[Unit]
Description=remoses — remote control of amateur radio transceivers
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=remoses
Group=remoses
SupplementaryGroups=dialout
ExecStart=$BINDIR/remoses -config /etc/remoses/remoses.yaml
Restart=on-failure
RestartSec=5

# It needs its serial ports and nothing else.
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
ReadWritePaths=

[Install]
WantedBy=multi-user.target
EOF

	$SUDO systemctl daemon-reload
	echo
	echo "the service is installed but NOT started, because the configuration"
	echo "still has the example passwords in it. When you have edited it:"
	echo
	echo "  sudo systemctl enable --now remoses"
	echo "  systemctl status remoses"
}

if [ "$WANT_SYSTEMD" = "1" ]; then
	setup_systemd
fi

# --- what next --------------------------------------------------------------

cat <<EOF

done. remoses $VERSION is installed.

  remoses -version
  remoses passwd                       generate a password hash for the config
  remoses -config remoses.yaml -check  validate a configuration
  remoses test-run                     exercise your radio and write a report

An annotated example configuration is in the archive as remoses.example.yaml,
and the user guide is at https://github.com/$REPO/tree/main/docs
EOF

if [ "$WANT_SYSTEMD" != "1" ]; then
	echo
	echo "To run it at boot on a machine by the radio, rerun with --systemd."
fi

case ":${PATH}:" in
*":$BINDIR:"*) ;;
*) echo "
NOTE: $BINDIR is not on your PATH." ;;
esac
