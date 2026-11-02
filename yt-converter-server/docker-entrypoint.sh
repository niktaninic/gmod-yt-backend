#!/bin/sh
set -eu

if [ "$(id -u)" = "0" ]; then
  # volumes created by the old (root) container are owned by root
  for d in /app/data /app/cache; do
    mkdir -p "$d"
    if [ "$(stat -c %u "$d")" != "10001" ]; then
      echo "entrypoint: fixing ownership of $d"
      chown -R app:app "$d"
    fi
  done

  # YouTube breaks old yt-dlp versions regularly; opt-in update on start
  case "${YTDLP_AUTO_UPDATE:-0}" in
    1|true|yes)
      echo "entrypoint: updating yt-dlp..."
      timeout 120 /opt/yt/bin/pip install --no-cache-dir --disable-pip-version-check -q -U "yt-dlp[default]" \
        || echo "entrypoint: yt-dlp update failed, keeping $(/opt/yt/bin/yt-dlp --version)"
      ;;
  esac

  # tini must run as the same user as the server, otherwise (with all
  # capabilities dropped) it may not forward SIGTERM on `docker stop`
  exec setpriv --reuid=app --regid=app --init-groups tini -- "$@"
fi

exec tini -- "$@"
