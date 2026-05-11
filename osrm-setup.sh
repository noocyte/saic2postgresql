#!/bin/bash
# OSRM Setup Script — One-time preparation of Norway road network data
# This downloads the Norway map extract and processes it for OSRM routing/matching.
#
# Run this script from the same directory as your docker-compose.yml:
#   chmod +x osrm-setup.sh
#   ./osrm-setup.sh
#
# After running, you should have an osrm-data/ directory ready for the OSRM container.
# Disk usage: ~1.5 GB for the processed data. Processing takes 5-15 minutes.

set -euo pipefail

OSRM_DIR="./osrm-data"
PBF_URL="https://download.geofabrik.de/europe/norway-latest.osm.pbf"
PBF_FILE="norway-latest.osm.pbf"
OSRM_IMAGE="osrm/osrm-backend"

echo "=== OSRM Setup for Norway ==="
echo ""

# Create data directory
mkdir -p "$OSRM_DIR"

# Download Norway map data
if [ -f "$OSRM_DIR/$PBF_FILE" ]; then
    echo "Map file already exists: $OSRM_DIR/$PBF_FILE"
    echo "Delete it and re-run to download a fresh copy."
else
    echo "Downloading Norway map data (~1 GB)..."
    wget -O "$OSRM_DIR/$PBF_FILE" "$PBF_URL"
    echo "Download complete."
fi
echo ""

# Step 1: Extract
echo "Step 1/3: Extracting road network (this takes a few minutes)..."
docker run --rm -t -v "$(pwd)/$OSRM_DIR:/data" "$OSRM_IMAGE" \
    osrm-extract -p /opt/car.lua /data/$PBF_FILE
echo "Extract complete."
echo ""

# Step 2: Partition
echo "Step 2/3: Partitioning road network..."
docker run --rm -t -v "$(pwd)/$OSRM_DIR:/data" "$OSRM_IMAGE" \
    osrm-partition /data/norway-latest.osrm
echo "Partition complete."
echo ""

# Step 3: Customize
echo "Step 3/3: Customizing road network..."
docker run --rm -t -v "$(pwd)/$OSRM_DIR:/data" "$OSRM_IMAGE" \
    osrm-customize /data/norway-latest.osrm
echo "Customize complete."
echo ""

echo "=== OSRM setup finished! ==="
echo ""
echo "You can now add the OSRM service to your docker-compose.yml:"
echo ""
echo "  osrm:"
echo "    image: osrm/osrm-backend"
echo "    restart: unless-stopped"
echo "    volumes:"
echo "      - ./osrm-data:/data"
echo "    command: osrm-routed --algorithm mld /data/norway-latest.osrm"
echo ""
echo "And add OSRM_URL to your saic2postgresql environment:"
echo ""
echo "    environment:"
echo "      - OSRM_URL=http://osrm:5000"
