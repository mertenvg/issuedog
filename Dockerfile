# syntax=docker/dockerfile:1

FROM golang:1.27.2-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/issuedog ./cmd/issuedog

# The npm package's postinstall swaps bin/claude.exe for the native binary of
# the build platform, so only that one file is copied into the runtime image.
FROM node:22-trixie-slim AS claude
ARG CLAUDE_CODE_VERSION=2.1.287
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
 && mkdir /out \
 && cp "$(npm root -g)/@anthropic-ai/claude-code/bin/claude.exe" /out/claude \
 && /out/claude --version | grep -F "${CLAUDE_CODE_VERSION}"

FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git iptables libcap2-bin \
 && rm -rf /var/lib/apt/lists/*

# Fixed uids and gids, so files on mounted volumes keep their owners across
# rebuilds.
RUN groupadd --system --gid 10001 issuedog \
 && groupadd --system --gid 10002 agent \
 && useradd --system --uid 10001 --gid issuedog --home-dir /work --no-create-home --shell /usr/sbin/nologin issuedog \
 && useradd --system --uid 10002 --gid agent --home-dir /home/agent --create-home --shell /usr/sbin/nologin agent \
 && chmod 0700 /home/agent \
 && install -d -o issuedog -g issuedog -m 0755 /work \
 && install -d -o agent -g agent -m 0755 /work/runs \
 && install -d -o issuedog -g issuedog -m 0700 /state \
 && install -d -o issuedog -g issuedog -m 0755 /run/issuedog

COPY --from=claude /out/claude /usr/local/bin/claude

# The daemon gets the only capabilities in the image: switching to agent for
# stage runs, and applying the agent firewall (#25). Only root and the
# issuedog group can run it, so agent can't borrow them. setcap comes last
# because chown clears file capabilities.
#
# The kernel refuses to start the binary unless the container grants every
# one of these, so run the image with --cap-add NET_ADMIN, and without
# --security-opt no-new-privileges, which ignores file capabilities.
COPY --from=build /out/issuedog /usr/local/bin/issuedog
RUN chown root:issuedog /usr/local/bin/issuedog \
 && chmod 0750 /usr/local/bin/issuedog \
 && setcap cap_setuid,cap_setgid,cap_net_admin+ep /usr/local/bin/issuedog

# Nothing in the container runs as root.
USER issuedog
ENV HOME=/work
ENTRYPOINT ["/usr/local/bin/issuedog"]
