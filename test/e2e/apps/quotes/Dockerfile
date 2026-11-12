# A deliberately broken app for the agent tests (test/e2e/smoke.sh, talk):
# it builds, but exits at start because its stack.yaml does not set
# QUOTES_FILE. The operator agent is to find that in its logs.
FROM alpine:3.22
COPY run.sh /run.sh
USER nobody
EXPOSE 8080
ENTRYPOINT ["/bin/sh", "/run.sh"]
