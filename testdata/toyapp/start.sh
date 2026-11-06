#!/bin/sh
# MODE: normal | crash | migrate-then-fail | bad-smoke | unhealthy
set -e
DATA=/data
cur=0
[ -f "$DATA/schema" ] && cur=$(cat "$DATA/schema")

if [ "$cur" -gt "$APP_VERSION" ]; then
  echo "data schema $cur is newer than this version $APP_VERSION: refusing to start" >&2
  exit 3
fi
if [ "$MODE" = crash ]; then
  echo "crashing on purpose" >&2
  exit 1
fi

if [ "$cur" -lt "$APP_VERSION" ]; then
  echo "$APP_VERSION" > "$DATA/schema"            # the forward-only migration
  echo "migrated-by-v$APP_VERSION" >> "$DATA/log"
fi
if [ "$MODE" = migrate-then-fail ]; then
  echo "failing after the migration" >&2
  exit 4
fi

mkdir -p /www
echo "v$APP_VERSION" > /www/version
case "$MODE" in
  unhealthy) ;;                                     # no /healthz: the healthcheck fails
  bad-smoke) echo degraded > /www/healthz ;;
  *)         echo ok > /www/healthz ;;
esac
printf 'server { listen 8080; root /www; }\n' > /etc/nginx/conf.d/default.conf
exec nginx -g 'daemon off;'
