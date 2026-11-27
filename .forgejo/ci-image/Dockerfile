# Waypoint's CI job image (WP-402, from WP-247's "saved snapshot" idea): what the
# heavy jobs in .forgejo/workflows/ci.yml install on every run, installed once.
#
#   - Go at go.mod's version and Node in /opt/hostedtoolcache, where setup-go and
#     setup-node look first, so they find it instead of downloading it (22-47 s a job);
#   - Playwright's Chromium and its system packages (109 apt packages, 141 s on every
#     e2e shard);
#   - the Makefile's Go QA tools (`make tools`) in ~/go/bin.
#
# build.sh builds it (`make ci-image`), passes every ARG, and tags it with a hash of
# the inputs. Nothing here has a default, so a build that goes around it fails.
#
# It stands in for the pool's own job image (Homelab/infrastructure
# .forgejo/runner/Dockerfile, which no registry holds): the same `runner` user (uid
# 1001, passwordless sudo) and the packages Waypoint's jobs use from it. Not its
# ansible or docker CLI: no Waypoint job runs them. Ubuntu 24.04, by digest: what
# the pool's image ran on 2026-11-02 (its apt said noble), and the newest release
# Playwright supports; on a newer one its install-deps guesses at package names.
FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ARG GO_VERSION
ARG GO_SHA256
ARG PLAYWRIGHT_VERSION
ARG IMAGE_TAG
# Node 24, as .nvmrc asks; the pool image's version and checksum (FORGEJO_CI_NODE_*).
ARG NODE_VERSION=24.22.2
ARG NODE_SHA256=48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75

# build-essential for cgo (the race detector), zstd and tar for the cache actions.
# Apt versions float with the build date, as on the pool's image and a hosted runner.
# hadolint ignore=DL3008
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl git gnupg jq openssh-client openssl \
      pkg-config python3 sudo tar unzip xz-utils zstd \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --uid 1001 --shell /bin/bash runner \
 && echo 'runner ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner \
 && install -d -o runner -g runner /opt/hostedtoolcache /opt/waypoint-ci /tmp/tools

# Go and Node in the tool cache layout setup-go and setup-node read: <tool>/<version>/x64
# plus an x64.complete marker. node is also on PATH: every JavaScript action runs on it.
RUN set -eu; \
    go=/opt/hostedtoolcache/go/${GO_VERSION:?build.sh passes this}/x64; \
    curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"; \
    echo "${GO_SHA256:?}  /tmp/go.tgz" | sha256sum -c -; \
    mkdir -p "$go"; tar -xzf /tmp/go.tgz -C "$go" --strip-components=1; touch "$go.complete"; \
    node=/opt/hostedtoolcache/node/${NODE_VERSION}/x64; \
    curl -fsSL -o /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c -; \
    mkdir -p "$node"; tar -xJf /tmp/node.tar.xz -C "$node" --strip-components=1; touch "$node.complete"; \
    ln -s "$node/bin/node" "$node/bin/npm" "$node/bin/npx" /usr/local/bin/; \
    chown -R runner:runner /opt/hostedtoolcache; \
    rm -f /tmp/go.tgz /tmp/node.tar.xz; \
    "$go/bin/go" version; node --version

# Chromium's system packages, as root; the browser itself goes in runner's home below.
RUN npx -y "playwright@${PLAYWRIGHT_VERSION:?}" install-deps chromium \
 && rm -rf /var/lib/apt/lists/* /root/.npm

USER runner
WORKDIR /home/runner
ENV PATH=/opt/hostedtoolcache/go/${GO_VERSION}/x64/bin:/home/runner/go/bin:$PATH

RUN npx -y "playwright@${PLAYWRIGHT_VERSION}" install chromium && rm -rf ~/.npm

# The tools the Makefile pins, built as `make tools` builds them; the module and
# build caches they leave are dropped (setup-go restores the repo's own).
WORKDIR /tmp/tools
COPY --chown=runner:runner Makefile Makefile
RUN make tools && go clean -cache -modcache && rm -rf ~/.cache/go-build

# What ci.yml asks of the image, so a job can tell what is baked in: the image's
# tag, the Playwright version, and go-tools' cache key (.forgejo/actions/go-tools).
RUN echo "${IMAGE_TAG:?}" > /opt/waypoint-ci/tag \
 && echo "${PLAYWRIGHT_VERSION}" > /opt/waypoint-ci/playwright \
 && echo "go-tools-$(go env GOVERSION)-$(grep -E '^[A-Z_]+_VERSION \?=' Makefile | sha256sum | cut -c1-16)" > /opt/waypoint-ci/go-tools \
 && rm -rf /tmp/tools
WORKDIR /home/runner
