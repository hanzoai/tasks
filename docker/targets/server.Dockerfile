# IMPORTANT: When updating ALPINE_TAG, also update the default value in:
# - docker/docker-bake.hcl (variable "ALPINE_TAG")
# - docker/targets/admin-tools.Dockerfile (ARG ALPINE_TAG)
# NOTE: We use just the tag without a digest pin because digest-pinned manifest lists
# cause platform resolution issues in multi-arch buildx builds (InvalidBaseImagePlatform warnings).
ARG ALPINE_TAG=3.23.3

FROM alpine:${ALPINE_TAG}

ARG TARGETARCH

RUN apk add --no-cache \
    ca-certificates \
    tzdata && addgroup -g 1000 tasks && \
    adduser -u 1000 -G tasks -D tasks

COPY --chmod=755 ./build/${TARGETARCH}/tasksd /usr/local/bin/
COPY --chmod=755 ./scripts/sh/entrypoint.sh /etc/tasks/entrypoint.sh

WORKDIR /etc/tasks
USER tasks

CMD [ "/etc/tasks/entrypoint.sh" ]
