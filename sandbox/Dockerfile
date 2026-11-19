# The agents' sandbox: one container per chat, started by the agent service
# (control-plane/internal/sandbox). Built with the control plane by its
# workflow; context: this folder.
FROM node:22-alpine3.22
# What agents work with besides Pi's own tools.
RUN apk add --no-cache bash git ripgrep fd curl jq
# Pictures and videos: cutting, joining and captions (the media agent).
RUN apk add --no-cache ffmpeg font-dejavu
# A headless browser for agents that reach the internet (the scout): the
# command web (web.mjs), without a sign-in.
RUN apk add --no-cache chromium
WORKDIR /opt/garrison
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund && ln -s /opt/garrison/node_modules/.bin/pi /usr/local/bin/pi
COPY garrison.ts start.sh web.mjs ./
RUN ln -s /opt/garrison/web.mjs /usr/local/bin/web && chmod 755 web.mjs
# The extension imports Pi's packages from here.
ENV NODE_PATH=/opt/garrison/node_modules PI_OFFLINE=1
# The image's own unprivileged user.
RUN mkdir -p /data && chown node:node /data
USER node
VOLUME /data
ENTRYPOINT ["/bin/sh", "/opt/garrison/start.sh"]
