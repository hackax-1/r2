#!/usr/bin/env bash
set -e

echo "=== Free CDN Relay Automated VPS Setup ==="

# 1. Install prerequisites
echo "[1/4] Installing FFmpeg, curl, and tools..."
apt-get update -y
apt-get install -y ffmpeg curl ufw

# 2. Deploy binary and configuration
echo "[2/4] Deploying cdn-relay..."
mkdir -p /opt/cdn-relay
cp cdn-relay-linux /opt/cdn-relay/cdn-relay
cp config.json /opt/cdn-relay/config.json
chmod +x /opt/cdn-relay/cdn-relay

# 3. Configure systemd service
echo "[3/4] Enabling systemd auto-restart service..."
cp deploy/cdn-relay.service /etc/systemd/system/cdn-relay.service
systemctl daemon-reload
systemctl enable --now cdn-relay

# 4. Status check
echo "[4/4] Verifying daemon health..."
sleep 2
systemctl status cdn-relay --no-pager

echo ""
echo "=== Deployment Complete ==="
echo "Relay active at http://localhost:8888"
echo "Test metrics: curl http://localhost:8888/api/status"
