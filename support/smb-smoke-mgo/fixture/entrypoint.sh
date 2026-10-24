#!/bin/sh
# The password is read from a read-only mounted file (SMB_SMOKE_PASSWORD_FILE,
# default /run/secrets/smb_password), never from the environment, so it does not
# show up in `docker inspect`. SMB_FIXTURE_MAX_PROTOCOL optionally pins the dialect.
set -eu
pwfile="${SMB_SMOKE_PASSWORD_FILE:-/run/secrets/smb_password}"
[ -s "$pwfile" ] || { echo "password file $pwfile missing or empty" >&2; exit 2; }
pw="$(cat "$pwfile")"
: > /etc/samba/dialect.conf
if [ -n "${SMB_FIXTURE_MAX_PROTOCOL:-}" ]; then
  printf '  server min protocol = %s\n  server max protocol = %s\n' \
    "$SMB_FIXTURE_MAX_PROTOCOL" "$SMB_FIXTURE_MAX_PROTOCOL" > /etc/samba/dialect.conf
fi
printf '%s\n%s\n' "$pw" "$pw" | smbpasswd -a -s smoke >/dev/null
unset pw
/usr/libexec/samba/samba-dcerpcd --libexec-rpcds --daemon
exec smbd --foreground --no-process-group --debug-stdout
