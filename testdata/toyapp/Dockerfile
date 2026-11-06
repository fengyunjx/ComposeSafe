# A tiny app for the upgrade/rollback integration tests. It behaves like an
# application with a forward-only data migration: a newer version rewrites the
# schema marker in /data, and an older version refuses to start on newer data.
# nginx serves the two files it writes (the Alpine busybox has no httpd).
ARG BASE=nginx:alpine
FROM ${BASE}
ARG APP_VERSION=1
ARG MODE=normal
ENV APP_VERSION=${APP_VERSION} MODE=${MODE}
RUN mkdir -p /data /www
COPY start.sh /app/start.sh
ENTRYPOINT []
CMD ["/app/start.sh"]
