#!/bin/sh
# Install any mounted git deploy key into the container user's ~/.ssh with the strict
# permissions and ownership OpenSSH requires. A read-only bind mount keeps the host's
# owner/mode, which ssh rejects ("bad ownership or modes"), so we copy rather than use
# the mount in place. Mount a dedicated key dir at /keys (see compose.yaml); on Fedora
# use the :Z SELinux relabel on that mount, never on your real ~/.ssh.
set -eu

SSH_DIR="$HOME/.ssh"
mkdir -p "$SSH_DIR"
chmod 700 "$SSH_DIR"

if [ -d /keys ]; then
	for k in /keys/id_*; do
		[ -e "$k" ] || continue          # no match: glob stays literal, skip
		case "$k" in *.pub) continue ;; esac
		dest="$SSH_DIR/$(basename "$k")"
		cp "$k" "$dest"
		chmod 600 "$dest"
	done
	if [ -f /keys/known_hosts ]; then
		cp /keys/known_hosts "$SSH_DIR/known_hosts"
		chmod 644 "$SSH_DIR/known_hosts"
	fi
	if [ -f /keys/config ]; then
		cp /keys/config "$SSH_DIR/config"
		chmod 600 "$SSH_DIR/config"
	fi
fi

exec snorgd "$@"
