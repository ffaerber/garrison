#!/bin/sh
# Serves the file QUOTES_FILE names as the page.
if [ -z "${QUOTES_FILE:-}" ]; then
  echo "quotes: cannot start: QUOTES_FILE is not set; set it to the file of quotes to serve" >&2
  exit 1
fi
mkdir -p /tmp/www && cp "$QUOTES_FILE" /tmp/www/index.html
exec httpd -f -p 8080 -h /tmp/www
