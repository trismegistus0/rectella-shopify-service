# Cloudflare Zero Trust Migration Guide

## Current State

- **Current tunnel**: Quick tunnel (`cloudflared tunnel --url http://localhost:9080`)
- **Issues**: 
  - No access controls - anyone with URL can reach public endpoints
  - URL changes on restart
  - No IP filtering for Shopify verification

## Zero Trust Setup Steps

### 1. Install cloudflared with Service Token

```bash
# Install cloudflared (if not already)
sudo pacman -S cloudflared

# Authenticate (run once)
cloudflared tunnel login
# This opens browser to authenticate with Cloudflare account
# Creates ~/.cloudflared/cert.pem
```

### 2. Create Named Tunnel

```bash
# Create a persistent tunnel
cloudflared tunnel create rectella-shopify

# This outputs a tunnel UUID, save it:
# TUNNEL_UUID: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
```

### 3. Create Config File

Create `/home/bast/.cloudflared/config.yml`:

```yaml
tunnel: <TUNNEL_UUID>
credentials-file: /home/bast/.cloudflared/<TUNNEL_UUID>.json

# Ingress rules - only expose webhook paths
ingress:
  # Health check - internal only (no hostname)
  - hostname: rectella-health.<your-domain>.com
    service: http://localhost:9080
    path: /health
    originRequest:
      noTLSVerify: true
    access:
      - team: <your-team-name>

  # Webhooks - public but with WAF rules
  - hostname: rectella-shopify.<your-domain>.com
    service: http://localhost:9080
    path: /webhooks/*
    originRequest:
      noTLSVerify: true
    # Note: Access policies don't apply to webhooks (Shopify can't auth)
    # Use WAF rules instead (see step 4)

  # Admin endpoints - require Cloudflare Access
  - hostname: rectella-admin.<your-domain>.com
    service: http://localhost:9080
    originRequest:
      noTLSVerify: true
    access:
      - team: <your-team-name>

  # Default - reject
  - service: http_status:404
```

### 4. Configure WAF Rules (Zero Trust Dashboard)

In Cloudflare Dashboard → Security → WAF:

```
# Rule 1: Block non-Shopify IPs to webhook endpoints
Expression:
  (http.host eq "rectella-shopify.<your-domain>.com" and
   http.request.uri.path contains "/webhooks/" and
   not ip.src in {23.227.38.0/24 185.145.157.0/24 185.145.156.0/24 185.145.158.0/24 185.145.159.0/24})
Action: Block

# Rule 2: Rate limiting for webhook endpoints
Expression:
  (http.host eq "rectella-shopify.<your-domain>.com" and
   http.request.uri.path contains "/webhooks/")
Action: Rate limit (100 req/min)

# Rule 3: Require user agent for webhooks
Expression:
  (http.host eq "rectella-shopify.<your-domain>.com" and
   http.request.uri.path contains "/webhooks/" and
   not http.user_agent contains "Shopify")
Action: Challenge (or Block after verification)
```

### 5. Create Access Policies (Zero Trust)

In Cloudflare Dashboard → Access → Applications:

1. **Application: rectella-health**
   - Session: 24 hours
   - Include: Email addresses (your email)
   - Exclude: None

2. **Application: rectella-admin**
   - Session: 1 hour
   - Include: Email addresses (your email)
   - Require: GitHub identity or OTP

### 6. Update Service Files

Replace `cloudflared.service`:

```ini
[Unit]
Description=Cloudflare Tunnel for Rectella Shopify
After=rectilla.service
Wants=rectilla.service

[Service]
Type=notify
ExecStart=/usr/bin/cloudflared tunnel --config /home/bast/.cloudflared/config.yml run
Restart=on-failure
RestartSec=10
Environment="TUNNEL_FORCE_PROVISIONING_DNS=true"

[Install]
WantedBy=default.target
```

### 7. DNS Setup

In Cloudflare Dashboard → DNS:

```
CNAME  rectilla-health    <TUNNEL_UUID>.cfargotunnel.com  Proxied
CNAME  rectilla-shopify   <TUNNEL_UUID>.cfargotunnel.com  Proxied
CNAME  rectilla-admin     <TUNNEL_UUID>.cfargotunnel.com  Proxied
```

### 8. Update Shopify Webhooks

```bash
# Update webhook URLs to new domain
# Orders/create: https://rectilla-shopify.<your-domain>.com/webhooks/orders/create
# Orders/cancelled: https://rectilla-shopify.<your-domain>.com/webhooks/orders/cancelled
```

### 9. Update Local Service

```bash
# Remove old quick tunnel service
systemctl --user stop cloudflared.service
systemctl --user disable cloudflared.service

# Enable new named tunnel service
systemctl --user daemon-reload
systemctl --user enable cloudflared.service
systemctl --user start cloudflared.service
```

## Security Benefits After Migration

| Feature | Quick Tunnel | Zero Trust Tunnel |
|---------|-------------|-------------------|
| URL persistence | ❌ Changes | ✅ Fixed domain |
| IP filtering | ❌ None | ✅ WAF rules |
| Access policies | ❌ None | ✅ Per-app policies |
| Rate limiting | ❌ None | ✅ WAF rate limits |
| Audit logging | ❌ None | ✅ Full request logs |
| Admin auth | ❌ Token only | ✅ SSO + MFA |

## Shopify Webhook IPs (for WAF allowlist)

Current Shopify webhook source IPs:
- `23.227.38.0/24`
- `185.145.156.0/24`
- `185.145.157.0/24`
- `185.145.158.0/24`
- `185.145.159.0/24`

Reference: https://shopify.dev/docs/apps/webhooks

## Post-Migration Verification

```bash
# Test health endpoint (should require auth)
curl https://rectilla-health.<your-domain>.com/health

# Test webhook (should work from Shopify IPs)
curl -H "X-Shopify-Topic: orders/create" \
     https://rectilla-shopify.<your-domain>.com/webhooks/orders/create

# Test admin (should redirect to Cloudflare Access)
curl -I https://rectilla-admin.<your-domain>.com/orders?status=pending
```
