#!/bin/sh
# Filebrowser on loopback, with its users named by the gate's header, and
# the gate on :8080. The media library is /srv, Filebrowser's own data
# /database. If either stops, the container stops, and Swarm starts it again.
set -eu
db=/database/filebrowser.db
if [ ! -f "$db" ]; then
  # Everyone may change files as far as Filebrowser knows: the gate refuses
  # changes from anyone not of the unit. No shares (public links past
  # the sign-in), no commands, no admins.
  /filebrowser config init -d "$db" --auth.method=proxy --auth.header=X-Garrison-User \
    --perm.create --perm.modify --perm.rename --perm.delete --perm.download \
    --perm.share=false --perm.execute=false --perm.admin=false \
    --branding.name "${FILES_NAME:-Media}" >/dev/null
fi
/filebrowser -d "$db" -a 127.0.0.1 -p 8081 -r /srv &
fb=$!
garrison gate -listen :8080 -upstream http://127.0.0.1:8081 &
gate=$!
while kill -0 "$fb" 2>/dev/null && kill -0 "$gate" 2>/dev/null; do sleep 5; done
echo "filebrowser or the gate stopped" >&2
exit 1
