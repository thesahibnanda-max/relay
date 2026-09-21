#!/bin/sh
# Regenerates THIRD_PARTY_NOTICES.md: the licence text of every module linked
# into the relay binary. BSD, MIT, ISC and Apache-2.0 all require these notices
# to accompany binary redistributions, so release archives include the file.
# Run after changing dependencies:  make notices
set -eu
GO=${GO:-go}
out=THIRD_PARTY_NOTICES.md
{
  echo "# Third-party notices"
  echo
  echo "Relay's own licence is in [LICENSE](LICENSE). The relay binary also contains the following"
  echo "third-party modules, each under the licence reproduced below."
  $GO list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./... | sort -u |
  while read -r path version dir; do
    echo
    echo "---"
    echo
    echo "## $path $version"
    for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
      [ -f "$f" ] || continue
      echo
      echo "### $(basename "$f")"
      echo
      echo '```text'
      cat "$f"
      echo '```'
    done
  done
} > "$out"
echo "wrote $out"
