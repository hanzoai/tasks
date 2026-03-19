variable "SERVER_VERSION" {
  default = "unknown"
}

variable "CLI_VERSION" {
  default = "unknown"
}

variable "IMAGE_REPO" {
  default = "ghcr.io/hanzoai"
}

variable "IMAGE_SHA_SHORT_TAG" {
  default = ""
}

variable "IMAGE_SHA_FULL_TAG" {
  default = ""
}

variable "IMAGE_BRANCH_TAG" {
  default = ""
}

variable "SAFE_IMAGE_BRANCH_TAG" {
  default = join("-", [for c in regexall("[a-z0-9]+", lower(IMAGE_BRANCH_TAG)) : c])
}

variable "TEMPORAL_SHA" {
  default = ""
}

variable "TAG_LATEST" {
  default = false
}

# IMPORTANT: When updating ALPINE_TAG, also update the default value in:
# - docker/targets/admin-tools.Dockerfile
# - docker/targets/server.Dockerfile
# NOTE: We use just the tag without a digest pin because digest-pinned manifest lists
# cause platform resolution issues in multi-arch buildx builds (InvalidBaseImagePlatform warnings).
variable "ALPINE_TAG" {
  default = "3.23.3"
}

target "admin-tools" {
  context = "docker"
  dockerfile = "targets/admin-tools.Dockerfile"
  args = {
    ALPINE_TAG = "${ALPINE_TAG}"
  }
  tags = compact([
    "${IMAGE_REPO}/tasks-admin-tools:${IMAGE_SHA_SHORT_TAG}",
    "${IMAGE_REPO}/tasks-admin-tools:${IMAGE_SHA_FULL_TAG}",
    "${IMAGE_REPO}/tasks-admin-tools:${SAFE_IMAGE_BRANCH_TAG}",
    TAG_LATEST ? "${IMAGE_REPO}/tasks-admin-tools:latest" : "",
  ])
  platforms = ["linux/amd64", "linux/arm64"]
  labels = {
    "org.opencontainers.image.title" = "tasks-admin-tools"
    "org.opencontainers.image.description" = "Hanzo Tasks admin tools"
    "org.opencontainers.image.url" = "https://github.com/hanzoai/tasks"
    "org.opencontainers.image.source" = "https://github.com/hanzoai/tasks"
    "org.opencontainers.image.licenses" = "MIT"
    "org.opencontainers.image.revision" = "${TEMPORAL_SHA}"
    "org.opencontainers.image.created" = timestamp()
    "com.hanzo.tasks.version" = "${SERVER_VERSION}"
    "com.hanzo.tasks.cli-version" = "${CLI_VERSION}"
  }
}

target "server" {
  context = "docker"
  dockerfile = "targets/server.Dockerfile"
  args = {
    ALPINE_TAG = "${ALPINE_TAG}"
  }
  tags = compact([
    "${IMAGE_REPO}/tasks:${IMAGE_SHA_SHORT_TAG}",
    "${IMAGE_REPO}/tasks:${IMAGE_SHA_FULL_TAG}",
    "${IMAGE_REPO}/tasks:${SAFE_IMAGE_BRANCH_TAG}",
    TAG_LATEST ? "${IMAGE_REPO}/tasks:latest" : "",
  ])
  platforms = ["linux/amd64", "linux/arm64"]
  labels = {
    "org.opencontainers.image.title" = "tasks"
    "org.opencontainers.image.description" = "Hanzo Tasks Server"
    "org.opencontainers.image.url" = "https://github.com/hanzoai/tasks"
    "org.opencontainers.image.source" = "https://github.com/hanzoai/tasks"
    "org.opencontainers.image.licenses" = "MIT"
    "org.opencontainers.image.version" = "${SERVER_VERSION}"
    "org.opencontainers.image.revision" = "${TEMPORAL_SHA}"
    "org.opencontainers.image.created" = timestamp()
    "com.hanzo.tasks.version" = "${SERVER_VERSION}"
  }
}
