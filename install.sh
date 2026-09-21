#!/bin/sh
# Relay installer for macOS, Linux and WSL.
#
#   curl -fsSL https://relay-sahib-nanda.vercel.app/install.sh | bash
#
# Downloads the latest release build for this machine, checks its checksum, installs
# it to ~/.local/bin and adds that folder to your PATH (in your shell's startup file).
#
# Options (environment variables):
#   RELAY_VERSION=v1.2.3        install this version instead of the latest
#   RELAY_INSTALL_DIR=/some/dir install here instead of ~/.local/bin
#   RELAY_NO_MODIFY_PATH=1      do not edit any shell startup file
set -eu

REPO=${RELAY_REPO:-thesahibnanda-max/relay}
BASE=${RELAY_DOWNLOAD_BASE:-https://github.com/$REPO/releases}
VERSION=${RELAY_VERSION:-latest}
DIR=${RELAY_INSTALL_DIR:-$HOME/.local/bin}

say() { printf '%s\n' "$*"; }
die() { printf 'relay install: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

have curl || die "curl is required (install it with your package manager and run this again)"
have tar || die "tar is required"

# --- which build does this machine need?
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN*) die "Windows is not supported natively: install WSL, then run this inside the WSL terminal" ;;
  *) die "unsupported system: $(uname -s) (Relay supports macOS, Linux and WSL)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac
# A terminal running under Rosetta on an Apple Silicon Mac reports x86_64; use the native build.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
  arch=arm64
fi

# --- which version?
if [ "$VERSION" = latest ]; then
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$BASE/latest") || die "could not reach $BASE/latest (is the repository public and does it have a release?)"
  tag=${url##*/}
  case "$tag" in v[0-9]*) ;; *) die "could not work out the latest version from $url" ;; esac
else
  tag=v${VERSION#v}
fi
ver=${tag#v}
archive="relay_${ver}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Installing relay $tag for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$BASE/download/$tag/$archive" || die "download failed: $BASE/download/$tag/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$BASE/download/$tag/checksums.txt" || die "could not download checksums.txt"

# --- verify the download
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
if have sha256sum; then
  got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
elif have shasum; then
  got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
else
  die "need sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || die "checksum mismatch for $archive (expected $want, got $got); nothing was installed"

# --- install
tar -xzf "$tmp/$archive" -C "$tmp" relay || die "could not unpack $archive"
mkdir -p "$DIR"
# Copy under a temporary name, then rename: safe even if relay is running right now.
cp "$tmp/relay" "$DIR/.relay.new.$$"
chmod 755 "$DIR/.relay.new.$$"
mv -f "$DIR/.relay.new.$$" "$DIR/relay"
say "Installed: $DIR/relay"

# --- make sure `relay` is found in new terminals
case ":$PATH:" in
  *":$DIR:"*) ;;
  *)
    if [ -n "${RELAY_NO_MODIFY_PATH:-}" ]; then
      say "Add $DIR to your PATH to run relay from anywhere."
    else
      shell_name=$(basename "${SHELL:-sh}")
      case "$shell_name" in
        zsh) rc=$HOME/.zshrc; line="export PATH=\"$DIR:\$PATH\"" ;;
        bash) if [ "$os" = darwin ]; then rc=$HOME/.bash_profile; else rc=$HOME/.bashrc; fi; line="export PATH=\"$DIR:\$PATH\"" ;;
        fish) rc=$HOME/.config/fish/config.fish; line="fish_add_path \"$DIR\"" ;;
        *) rc=$HOME/.profile; line="export PATH=\"$DIR:\$PATH\"" ;;
      esac
      if [ -f "$rc" ] && grep -qF "# added by relay installer" "$rc"; then
        say "PATH already set up in $rc"
      else
        mkdir -p "$(dirname "$rc")"
        printf '\n# added by relay installer\n%s\n' "$line" >> "$rc"
        say "Added $DIR to your PATH in $rc"
      fi
      say "Open a new terminal, or run:  export PATH=\"$DIR:\$PATH\""
    fi
    ;;
esac

"$DIR/relay" version || true
say "Next: run  relay doctor  to check everything is in order."
