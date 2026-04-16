#!/bin/bash
# Captures all SAIC MQTT messages to a file via a temporary Docker container.
# Usage: ./mqtt_capture.sh [network] [broker_host]
#   network:     Docker network name (default: teslamate_default)
#   broker_host: MQTT broker hostname (default: mosquitto)
#
# Output is written to ./capture/mqtt_capture.txt
# Press Ctrl+C to stop.

NETWORK="${1:-teslamate_default}"
BROKER="${2:-mosquitto}"
TOPIC="saic/#"

mkdir -p "$(pwd)/capture"

echo "Connecting to broker '$BROKER' on network '$NETWORK'..."
echo "Subscribing to '$TOPIC' — press Ctrl+C to stop."
echo ""

docker run --rm -it \
  --network="$NETWORK" \
  -v "$(pwd)/capture:/capture" \
  eclipse-mosquitto \
  sh -c "mosquitto_sub -h '$BROKER' -v -t '$TOPIC' | tee /capture/mqtt_capture.txt"
