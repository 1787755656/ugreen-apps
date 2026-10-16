FROM alpine:3.20

RUN apk add --no-cache python3 curl

ENV UGTHEME_PORT=28750 \
    CRON_SCHEDULE="0 8 * * *"

WORKDIR /app

COPY --chmod=755 app/server.py app/wallpaper.py /app/
COPY --chmod=755 app/entrypoint.sh /entrypoint.sh
COPY app/icons /app/icons
COPY app/web /app/web

EXPOSE 28750

ENTRYPOINT ["/entrypoint.sh"]
