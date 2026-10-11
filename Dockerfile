# syntax=docker/dockerfile:1

FROM golang:1.26.8-trixie AS build
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

# Only root and the issuedog group can run the daemon, so agent can't borrow
# its capabilities. setcap comes last because chown clears file capabilities.
COPY --from=build /out/issuedog /usr/local/bin/issuedog
RUN chown root:issuedog /usr/local/bin/issuedog \
 && chmod 0750 /usr/local/bin/issuedog \
 && setcap cap_setuid,cap_setgid+ep /usr/local/bin/issuedog

COPY --chmod=0755 sandbox/entrypoint.sh /usr/local/lib/issuedog/entrypoint.sh

# The container starts as root so the entrypoint can apply the firewall.
ENTRYPOINT ["/usr/local/lib/issuedog/entrypoint.sh"]
