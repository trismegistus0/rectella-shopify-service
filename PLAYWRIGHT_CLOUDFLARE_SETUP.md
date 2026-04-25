# Cloudflare Zero Trust Setup - Playwright Automation Prompt

## Context

**Current State:**
- Quick tunnel running: `cloudflared tunnel --url http://localhost:9080`
- Tunnel URL: `https://sally-reductions-symbol-accounting.trycloudflare.com` (temporary, changes on restart)
- Service: Rectella Shopify integration running on localhost:9080
- Need: Named tunnel with Zero Trust security

**Requirements:**
1. Create named Cloudflare tunnel
2. Configure WAF rules for Shopify IP allowlist
3. Set up Access policies for admin endpoints
4. Create DNS records
5. Update Shopify webhooks

---

## Playwright Automation Prompt

Copy-paste this into Claude Code with Playwright:

```
I need you to help me migrate from a Cloudflare Quick Tunnel to a Cloudflare Zero Trust named tunnel with proper security controls. I have the Playwright MCP plugin available.

## Prerequisites

First, check if cloudflared CLI is authenticated:
```bash
ls -la ~/.cloudflared/cert.pem
cloudflared tunnel list
```

If not authenticated, I'll need to run `cloudflared tunnel login` first (opens browser).

## Tunnel Details

- **Tunnel name:** rectilla-shopify
- **Domain:** [YOUR_DOMAIN] (e.g., rectilla.com)
- **Local service:** http://localhost:9080
- **Endpoints to expose:**
  - `/webhooks/orders/create` - Shopify order webhooks (public with IP filter)
  - `/webhooks/orders/cancelled` - Shopify cancellation webhooks (public with IP filter)
  - `/health` - Health check (protected by Cloudflare Access)
  - `/orders` - Admin endpoint (protected by Cloudflare Access)

## Shopify Webhook IPs (for WAF allowlist):
- 23.227.38.0/24
- 185.145.156.0/24
- 185.145.157.0/24
- 185.145.158.0/24
- 185.145.159.0/24

## Tasks

### Phase 1: CLI Tunnel Creation (Bash tool)

Use the Bash tool to:

1. Create the named tunnel if it doesn't exist:
```bash
cloudflared tunnel create rectilla-shopify 2>&1 | tee /tmp/tunnel-create.log
```

2. Extract the tunnel UUID:
```bash
cloudflared tunnel list | grep rectilla-shopify | awk '{print $1}'
```

3. Create the config file at `~/.cloudflared/config.yml`:
```yaml
tunnel: <TUNNEL_UUID>
credentials-file: /home/bast/.cloudflared/<TUNNEL_UUID>.json

ingress:
  - hostname: rectilla-shopify.<DOMAIN>
    service: http://localhost:9080
    originRequest:
      noTLSVerify: true
  - service: http_status:404
```

4. Create DNS record:
```bash
cloudflared tunnel route dns rectilla-shopify "rectilla-shopify.<DOMAIN>"
```

### Phase 2: Cloudflare Dashboard Configuration (Playwright)

Use the Playwright MCP browser tools to:

1. **Navigate to Cloudflare Dashboard**
   - URL: https://dash.cloudflare.com
   - Wait for login or already authenticated state
   - Screenshot to confirm access

2. **Select the correct domain**
   - Look for domain: [YOUR_DOMAIN]
   - Click to enter the zone

3. **Configure WAF Rules** (Security → WAF → Custom Rules)
   
   Create rule: "Allow Shopify Webhooks Only"
   
   Navigate to: Security → WAF → Custom rules → Create rule
   
   Fill in:
   - Rule name: `Allow Shopify Webhooks Only`
   - Expression: `(http.host eq "rectilla-shopify.<DOMAIN>" and not ip.src in {23.227.38.0/24 185.145.156.0/24 185.145.157.0/24 185.145.158.0/24 185.145.159.0/24})`
   - Action: Block
   - Deploy
   
   Take screenshots at each step.

4. **Create Access Application** (Zero Trust → Access → Applications)
   
   Navigate to: Zero Trust (sidebar) → Access → Applications → Add an application
   
   Configure:
   - Application name: `Rectilla Admin`
   - Session duration: 1 hour
   - Domain: `rectilla-shopify.<DOMAIN>`
   - Policies:
     - Name: `Admin Access`
     - Include: Email addresses (add your email)
     - Require: GitHub or OTP
   
   Take screenshots.

5. **Verify DNS Records** (DNS → Records)
   
   Navigate to: DNS → Records
   
   Verify CNAME exists:
   - Name: `rectilla-shopify`
   - Target: `<TUNNEL_UUID>.cfargotunnel.com`
   - Proxied: Yes (orange cloud)

### Phase 3: Shopify Webhook Update (Playwright)

1. **Navigate to Shopify Admin**
   - URL: https://admin.shopify.com
   - Login if needed
   - Select store: [YOUR_STORE]

2. **Update Webhooks** (Settings → Notifications → Webhooks OR Settings → Apps and sales channels → Develop apps)
   
   Navigate to webhook settings.
   
   Update these endpoints:
   - Orders/create: `https://rectilla-shopify.<DOMAIN>/webhooks/orders/create`
   - Orders/cancelled: `https://rectilla-shopify.<DOMAIN>/webhooks/orders/cancelled`
   
   Take screenshots before and after.

### Phase 4: Service Migration (Bash)

Use Bash tool to:

1. Stop the old quick tunnel service:
```bash
systemctl --user stop cloudflared.service
systemctl --user disable cloudflared.service
```

2. Update the service file to use named tunnel:
```bash
cat > ~/.config/systemd/user/cloudflared.service << 'EOF'
[Unit]
Description=Cloudflare Tunnel for Rectilla Shopify (Zero Trust)
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
EOF
```

3. Reload and start:
```bash
systemctl --user daemon-reload
systemctl --user enable cloudflared.service
systemctl --user start cloudflared.service
```

4. Verify:
```bash
systemctl --user status cloudflared.service
cloudflared tunnel list
```

### Phase 5: Verification

1. Test health endpoint (should require auth now):
```bash
curl -s https://rectilla-shopify.<DOMAIN>/health
```

2. Test webhook endpoint (should work with Shopify HMAC):
```bash
# Simulate Shopify request
curl -X POST https://rectilla-shopify.<DOMAIN>/webhooks/orders/create \
  -H "X-Shopify-Topic: orders/create" \
  -H "X-Shopify-Hmac-SHA256: test" \
  -d '{"test":"data"}'
```

3. Check tunnel status:
```bash
cloudflared tunnel info rectilla-shopify
```

## Expected Outcomes

After completion:
- ✅ Named tunnel with persistent URL
- ✅ WAF rules blocking non-Shopify IPs on webhooks
- ✅ Cloudflare Access protecting admin endpoints
- ✅ Updated Shopify webhook URLs
- ✅ Service running with new config

## Screenshots Required

Please capture screenshots of:
1. WAF rule creation
2. Access application configuration
3. DNS record verification
4. Shopify webhook settings (before/after)
5. Tunnel status in dashboard

## Troubleshooting Notes

If Playwright can't access Cloudflare dashboard:
- Check if already logged in (auth cookies)
- If 2FA required, pause and notify me
- Some actions may need to be done manually

If tunnel creation fails:
- Check `cloudflared tunnel list` for existing tunnels
- May need to delete old tunnel first: `cloudflared tunnel delete <name>`

Shopify webhook verification:
- Shopify will verify HMAC signature automatically
- If webhooks fail, check WAF logs for blocked IPs
```

---

## Quick Start

To use this prompt:

1. Replace `[YOUR_DOMAIN]` with your actual domain (e.g., `rectilla.com`)
2. Replace `[YOUR_STORE]` with your Shopify store name
3. Ensure you're logged into Cloudflare in your browser
4. Ensure you have access to your Shopify admin
5. Run the prompt in Claude Code with Playwright plugin enabled

The prompt will handle both CLI operations (via Bash tool) and browser automation (via Playwright).
