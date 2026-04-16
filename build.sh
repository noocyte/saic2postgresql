#!/bin/bash
set -euo pipefail

# --- Configuration ---
REPO="noocyte/saic2postgresql"
PLATFORMS="linux/amd64,linux/arm64"

# --- Determine version tag ---
# Use first argument, or git tag, or "latest"
if [[ -n "${1:-}" ]]; then
    VERSION="$1"
elif git describe --tags --exact-match HEAD 2>/dev/null; then
    VERSION=$(git describe --tags --exact-match HEAD)
else
    VERSION="latest"
fi

echo "==> Building ${REPO}:${VERSION}"
echo "    Platforms: ${PLATFORMS}"

# --- Build manifest for each platform ---
MANIFEST="${REPO}:${VERSION}"
IMAGES=()

IFS=',' read -ra PLATS <<< "$PLATFORMS"
for PLAT in "${PLATS[@]}"; do
    OS="${PLAT%%/*}"
    ARCH="${PLAT##*/}"
    PLAT_TAG="${VERSION}-${ARCH}"

    echo "==> Building for ${PLAT}..."
    podman build --network=host \
        --platform "$PLAT" \
        --tag "${REPO}:${PLAT_TAG}" \
        .

    echo "==> Pushing ${REPO}:${PLAT_TAG}..."
    podman push "${REPO}:${PLAT_TAG}"

    IMAGES+=("${REPO}:${PLAT_TAG}")
done

# --- Create and push manifest ---
echo "==> Creating manifest ${MANIFEST}..."
podman manifest create "${MANIFEST}" "${IMAGES[@]}"
podman manifest push "${MANIFEST}" "docker://${MANIFEST}"

if [[ "$VERSION" != "latest" ]]; then
    echo "==> Tagging as ${REPO}:latest..."
    podman manifest create "${REPO}:latest" "${IMAGES[@]}"
    podman manifest push "${REPO}:latest" "docker://${REPO}:latest"
fi

# --- Cleanup per-arch tags ---
for IMG in "${IMAGES[@]}"; do
    podman rmi "$IMG" 2>/dev/null || true
done

echo "==> Pushed ${MANIFEST}"
if [[ "$VERSION" != "latest" ]]; then
    echo "    Also tagged as ${REPO}:latest"
fi
