#!/bin/sh
# The package never enables or restarts the user service: a package cannot act on each
# person's user manager, and the service needs `jiku login` first. It only says so.
cat <<'MSG'

jiku: the docs sync runs as a systemd user service. To start it (once per person):

    jiku login
    systemctl --user enable --now jiku-docs-sync

If it was already running, take the new version with:

    systemctl --user daemon-reload && systemctl --user restart jiku-docs-sync

(or log out and back in).

MSG
exit 0
