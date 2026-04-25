#!/bin/bash
# Setup Cloudflare Zero Trust tunnel for Rectella Shopify Service
# Run this after: cloudflared tunnel login

set -euo pipefail

TUNNEL_NAME="rectella-shopify"
CONFIG_DIR="/home/bast/.cloudflared"
DOMAIN=""  # Set your domain here, e.g., "yourdomain.com"
TEAM_NAME=""  # Set your Cloudflare team name here

if [[ -z "$DOMAIN" ]]; then
    echo "ERROR: Please edit this script and set DOMAIN to your Cloudflare domain"
    exit 1
fi

if [[ -z "$TEAM_NAME" ]]; then
    echo "ERROR: Please edit this script and set TEAM_NAME to your Cloudflare team name"
    exit 1
fi

echo "=== Creating Cloudflare Zero Trust Tunnel ==="

# Check if already authenticated
if [[ ! -f "$CONFIG_DIR/cert.pem" ]]; then
    echo "ERROR: Not authenticated. Run: cloudflared tunnel login"
    exit 1
fi

# Create tunnel if doesn't exist
if [[ ! -f "$CONFIG_DIR/${TUNNEL_NAME}.json" ]]; then
    echo "Creating tunnel: $TUNNEL_NAME"
    cloudflared tunnel create "$TUNNEL_NAME"
else
    echo "Tunnel already exists: $TUNNEL_NAME"
fi

# Get tunnel UUID
TUNNEL_UUID=$(cloudflared tunnel list | grep "$TUNNEL_NAME" | awk '{print $1}')
echo "Tunnel UUID: $TUNNEL_UUID"

# Create config
echo "Creating config file..."
cat > "$CONFIG_DIR/config.yml" << EOF
tunnel: $TUNNEL_UUID
credentials-file: $CONFIG_DIR/$TUNNEL_UUID.json

# Only expose webhook endpoints publicly
# Admin and health endpoints protected by Cloudflare Access
ingress:
  # Webhook endpoints - public with WAF rules for Shopify IPs
  - hostname: rectella-shopify.$DOMAIN
    service: http://localhost:9080
    originRequest:
      noTLSVerify: true

  # Default - reject all other paths
  - service: http_status:404
EOF

echo "Config created at: $CONFIG_DIR/config.yml"

# Create DNS records
echo "Creating DNS records..."
cloudflared tunnel route dns "$TUNNEL_NAME" "rectella-shopify.$DOMAIN" || echo "DNS record may already exist"

echo ""
echo "=== Next Steps ==="
echo "1. Go to Cloudflare Dashboard: https://dash.cloudflare.com"
echo "2. Navigate to: Security → WAF → Custom Rules"
echo "3. Add this rule for Shopify webhook protection:"
echo ""
echo "   Rule Name: Allow Shopify Webhooks Only"
echo "   Expression: |"
echo "     (http.host eq \"rectella-shopify.$DOMAIN\" and"
echo "      not ip.src in {23.227.38.0/24 185.145.157.0/24 185.145.156.0/24})"
echo "   Action: Block"
echo ""
echo "4. Update your Shopify webhook URLs to:"
echo "   https://rectella-shopify.$DOMAIN/webhooks/orders/create"
echo "   https://rectella-shopify.$DOMAIN/webhooks/orders/cancelled"
echo ""
echo "5. Replace the cloudflared service:"
echo "   systemctl --user stop cloudflared.service"
echo "   systemctl --user disable cloudflared.service"
echo "   # Edit ~/.config/systemd/user/cloudflared.service to use:"
echo "   # ExecStart=/usr/bin/cloudflared tunnel --config /home/bast/.cloudflared/config.yml run"
echo ""
echo "6. Start the new tunnel:"
echo "   systemctl --user daemon-reload"
echo "   systemctl --user start cloudflared.service"
