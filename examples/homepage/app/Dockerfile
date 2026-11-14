# The homepage: one static page, served by nginx as an unprivileged user on
# port 8080. Built by BuildKit for every push (.gitea/workflows/pipeline.yaml).
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY index.html /usr/share/nginx/html/index.html
EXPOSE 8080
