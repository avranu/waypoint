# opencode as Waypoint's AI provider: `opencode serve` on the compose network
# only (no published port). It brings opencode's free models; sign in to
# more providers with:
#   docker compose -p waypoint -f deploy/compose.yaml exec opencode opencode auth login
# Waypoint refuses every tool permission the agent asks for, and the agent's
# tools could only touch this container anyway.
# opencode-ai's own dependencies aren't locked, so --before keeps each of them
# at a version published at least 7 days before the build (WP-844; the pin
# itself is checked by cmd/dep-age through renovate.json).
FROM node:24-slim
ARG OPENCODE_VERSION=1.18.32
RUN npm install -g --no-audit --no-fund --before="$(date -u -d '-7 days' +%Y-%m-%dT%H:%M:%SZ)" "opencode-ai@${OPENCODE_VERSION}" \
 && useradd --create-home --uid 10001 opencode \
 && mkdir -p /home/opencode/work /home/opencode/.local/share/opencode /home/opencode/.config/opencode \
 && chown -R opencode:opencode /home/opencode
USER opencode
WORKDIR /home/opencode/work
EXPOSE 4096
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s \
  CMD ["node", "-e", "fetch('http://127.0.0.1:4096/global/health',{headers:{Authorization:'Basic '+btoa('opencode:'+(process.env.OPENCODE_SERVER_PASSWORD||''))}}).then(r=>process.exit(r.ok?0:1),()=>process.exit(1))"]
ENTRYPOINT ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096", "--pure"]
