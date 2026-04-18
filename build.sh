#!/bin/bash
set -euo pipefail

# --- Configuration ---
REPO="noocyte/saic2postgresql"
PLATFORMS="linux/amd64,linux/arm64"
VERSION_FILE="$(dirname "$0")/VERSION"

# --- Read and bump version ---
# Usage: ./build.sh          → patch bump (0.1.0 → 0.1.1)
#        ./build.sh minor    → minor bump (0.1.1 → 0.2.0)
#        ./build.sh major    → major bump (0.2.0 → 1.0.0)
BUMP="${1:-patch}"

CURRENT=$(cat "$VERSION_FILE")
IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT"

case "$BUMP" in
    major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
    minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
    patch) PATCH=$((PATCH + 1)) ;;
    *) echo "Usage: $0 [patch|minor|major]"; exit 1 ;;
esac

VERSION="v${MAJOR}.${MINOR}.${PATCH}"
echo "${MAJOR}.${MINOR}.${PATCH}" > "$VERSION_FILE"

echo "==> Version: ${CURRENT} → ${VERSION}"
echo "==> Building ${REPO}:${VERSION}"
echo "    Platforms: ${PLATFORMS}"

# --- Build each platform ---
MANIFEST="${REPO}:${VERSION}"
IMAGES=()

IFS=',' read -ra PLATS <<< "$PLATFORMS"
for PLAT in "${PLATS[@]}"; do
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

# --- Create and push manifests ---
echo "==> Creating manifest ${MANIFEST}..."
podman manifest create --replace "${MANIFEST}" "${IMAGES[@]}"
podman manifest push "${MANIFEST}" "docker://${MANIFEST}"

echo "==> Tagging as ${REPO}:latest..."
podman manifest create --replace "${REPO}:latest" "${IMAGES[@]}"
podman manifest push "${REPO}:latest" "docker://${REPO}:latest"

# --- Cleanup per-arch tags ---
for IMG in "${IMAGES[@]}"; do
    podman rmi "$IMG" 2>/dev/null || true
done

# --- Git tag ---
git add "$VERSION_FILE"
git commit -m "release: ${VERSION}"
git tag -a "${VERSION}" -m "Release ${VERSION}"
git push origin master "${VERSION}"

echo ""
echo "==> Done! Pushed ${REPO}:${VERSION} + ${REPO}:latest"
