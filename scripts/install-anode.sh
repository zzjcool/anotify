#!/bin/sh
# Download/verify only. All installation logic belongs to anode install.
set -eu
VERSION=${ANODE_VERSION:-}
ARCH=${ANODE_ARCH:-}
URL=${ANODE_DOWNLOAD_URL:-}
CHECKSUM_URL=${ANODE_CHECKSUM_URL:-}
BASE=${ANODE_DOWNLOAD_BASE:-https://github.com/zzjcool/anotify/releases/download}
while [ "$#" -gt 0 ]; do
 case "$1" in
  --version|--arch|--download-url|--checksum-url)
   [ "$#" -ge 2 ] || { echo "missing value for $1" >&2; exit 2; }
   case "$1" in --version) VERSION=$2;; --arch) ARCH=$2;; --download-url) URL=$2;; --checksum-url) CHECKSUM_URL=$2;; esac
   shift 2;;
  --) shift; break;;
  *) break;;
 esac
done
case "$VERSION" in ''|*[!A-Za-z0-9._-]*) echo 'an explicit safe --version / ANODE_VERSION is required' >&2; exit 2;; esac
[ "$(uname -s)" = Linux ] || { echo 'Linux required' >&2; exit 2; }
if [ -z "$ARCH" ]; then case "$(uname -m)" in x86_64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) echo 'unsupported architecture' >&2; exit 2;; esac; fi
case "$ARCH" in amd64|arm64) ;; *) echo 'unsupported architecture' >&2; exit 2;; esac
[ -n "$URL" ] || URL="$BASE/$VERSION/anode_linux_$ARCH"
[ -n "$CHECKSUM_URL" ] || CHECKSUM_URL="$URL.sha256"
TMP=$(mktemp -d)
cleanup() { rm -f "$TMP/anode" "$TMP/checksum"; rmdir "$TMP"; }
trap cleanup EXIT HUP INT TERM
curl --fail --location --connect-timeout 10 --max-time 60 --output "$TMP/anode" "$URL"
curl --fail --location --connect-timeout 10 --max-time 60 --output "$TMP/checksum" "$CHECKSUM_URL"
# Never evaluate a remote checksum filename/path. Accept exactly one digest.
SHA=$(awk 'NF {n++; s=$1} END {if(n!=1) exit 1; print s}' "$TMP/checksum")
case "$SHA" in *[!a-fA-F0-9]*|'') echo 'invalid checksum' >&2; exit 2;; esac
[ "${#SHA}" -eq 64 ] || { echo 'invalid checksum length' >&2; exit 2; }
printf '%s  %s\n' "$SHA" "$TMP/anode" | sha256sum --check --status
chmod 0700 "$TMP/anode"
INFO=$("$TMP/anode" --version)
printf '%s\n' "$INFO" | grep -F '"version":"'"$VERSION"'"' >/dev/null || { echo 'downloaded binary version mismatch' >&2; exit 2; }
# No YAML, units, systemctl, sudo, token generation or automatic deployment here.
"$TMP/anode" install "$@"
