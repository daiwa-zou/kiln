# The runtime image plus the Claude Code CLI, for the local Kubernetes stack.
#
# kiln's `agent.runner = "cli"` shells out to `claude -p` instead of calling the
# Messages API directly. The production image in the repository root does not
# ship that binary on purpose -- a deployment running the API runner should not
# carry a Node runtime it never executes -- so the local stack layers it on here
# rather than widening the image everybody else pulls.
#
# Built by scripts/k8s-local.sh, which builds the base image first and passes it
# as BASE_IMAGE.

ARG BASE_IMAGE=kiln:local

# The CLI is distributed on npm, so it arrives with a Node runtime attached.
# Taking both from the official Node image rather than apt keeps the version
# explicit and avoids Debian's older packaging.
FROM node:22-bookworm-slim AS claude
ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

FROM ${BASE_IMAGE} AS runtime

USER root

# node links against libstdc++, which debian-slim does not carry by default.
RUN apt-get update \
    && apt-get install --no-install-recommends -y libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

# Both trees wholesale: the CLI vendors platform-specific helpers (ripgrep among
# them) inside its own package directory, and cherry-picking paths breaks
# quietly on the next release that moves one. Copying into existing directories
# merges, so /usr/local/bin/kiln survives.
COPY --from=claude /usr/local/bin/ /usr/local/bin/
COPY --from=claude /usr/local/lib/node_modules/ /usr/local/lib/node_modules/

# Fail the build rather than a run if the CLI did not survive the copy.
RUN claude --version

USER 65532:65532

# The CLI writes state under $HOME and stages work under $TMPDIR, and the pods
# run with a read-only root filesystem. Both paths are emptyDir mounts in the
# manifests; declaring them here keeps `docker run` on this image working too.
ENV HOME=/home/kiln \
    TMPDIR=/tmp
