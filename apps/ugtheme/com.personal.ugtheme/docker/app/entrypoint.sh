#!/bin/sh
set -e
mkdir -p /data/logs /data/backup/icons /data/wallpaper /etc/crontabs
crond -l 8
exec python3 /app/server.py
