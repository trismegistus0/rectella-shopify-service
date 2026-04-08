# Azure Bicep Infrastructure — Design Spec

Date: 29 March 2026
Status: Draft

## Problem

The Rectella Shopify Service needs production infrastructure on Azure. Go-live is 31 March 2026. Currently there is no Azure provisioning — infrastructure is on the critical path. Manual portal setup under time pressure is error-prone and undocumented. IaC via Bicep gives a repeatable, reviewable, one-command deployment.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│  Azure Resource Group: rg-rectella-prod (UK South)      │
│                                                         │
│  ┌──────────────────────────────────────────────┐       │
│  │  VNet: 172.28.0.0/16                         │       │
│  │                                              │       │
│  │  ┌─────────────────┐  ┌──────────────────┐   │       │
│  │  │ Container Apps   │  │ PostgreSQL       │   │       │
│  │  │ Environment      │  │ Flexible Server  │   │       │
│  │  │ 172.28.1.0/23    │  │ 172.28.4.0/24    │   │       │
│  │  │                  │  │                  │   │       │
│  │  │  ┌────────────┐  │  │ rectella-db      │   │       │
│  │  │  │ rectella-  │  │  │ Burstable B1ms   │   │       │
│  │  │  │ shopify-svc│──┼──│ PG 16, 32GB      │   │       │
│  │  │  └────────────┘  │  └──────────────────┘   │       │
│  │  └────────┬─────────┘                         │       │
│  │           │                                   │       │
│  │  ┌────────┴─────────┐  ┌──────────────────┐   │       │
│  │  │ GatewaySubnet    │  │ ACR              │   │       │
│  │  │ 172.28.255.0/27  │  │ rectellaacr      │   │       │
│  │  │                  │  │ Basic SKU        │   │       │
│  │  │  VPN Gateway     │  └──────────────────┘   │       │
│  │  │  VpnGw1          │                         │       │
│  │  └────────┬─────────┘                         │       │
│  └───────────┼──────────────────────────────────-┘       │
│              │ IPsec/IKEv2 tunnel                        │
└──────────────┼───────────────────────────────────────────┘
               │
     ┌─────────┴─────────┐
     │ Rectella Meraki    │
     │ 192.168.3.0/24     │
     │                    │
     │  RIL-APP01 (.150)  │
     │  RIL-DB01  (.151)  │
     └────────────────────┘
```

### Key Decisions

- **VNet 172.28.0.0/16**: Uncommon range in RFC 1918 172.16.0.0/12 block. Avoids clashing with Rectella 192.168.3.0/24 or any 10.x VPN config.
- **Container Apps Environment** gets /23 (Azure minimum requirement for its internal infrastructure).
- **GatewaySubnet** must be named exactly GatewaySubnet (Azure hard requirement for VPN Gateway).
- **PostgreSQL** on private VNet access — not exposed to internet.
- **ACR Basic** — cheapest tier, sufficient for single image with infrequent pushes.
- **Auto-generated FQDN** for the Container App — no custom domain initially. Can be added later without code changes.
- **VPN Gateway VpnGw1** — minimum SKU supporting site-to-site (Basic is being retired by Azure).
- **UK South (London)** region — lowest latency to Rectella Burnley network.
- **No SYSPRO keep-alive session**: Stateless logon-per-batch model. Batch processor polls every 5m, logs in once per batch, submits N orders, logs off. Stock sync similar at 15m intervals. The approx 500ms logon cost is noise at batch boundaries. Simpler = more reliable (no stale session bugs, Container Apps can restart freely).

### Cost Estimate

| Component | SKU | Est. Monthly |
|-----------|-----|-------------|
| Container Apps | Consumption (min 1 replica) | 15 to 30 GBP |
| PostgreSQL Flexible Server | Burstable B1ms (1 vCore, 2GB) | 10 to 15 GBP |
| VPN Gateway | VpnGw1 | approx 25 GBP |
| Container Registry | Basic | approx 4 GBP |
| Public IP (VPN) | Standard | approx 3 GBP |
| **Total** | | **approx 57 to 77 GBP/month** |

Within the SOW budget of 80 to 150 GBP/month.

## File Structure

```
infra/
  main.bicep                # Orchestrator — calls modules, wires outputs
  modules/
    network.bicep           # VNet, subnets, NSGs
    postgres.bicep          # PostgreSQL Flexible Server + DB + firewall
    container-apps.bicep    # Container Apps Environment + app definition
    registry.bicep          # Azure Container Registry
    vpn.bicep               # VPN Gateway + local network gateway + connection
  parameters/
    prod.bicepparam         # Production values (non-secret)
  README.md                 # How to deploy, prerequisites, troubleshooting
```

### Why Modular

- **VPN is independently deployable.** NCS may not be ready on day one. Deploy everything else first, add VPN when they have configured the Meraki side. The service cannot reach SYSPRO without VPN, but the container + DB + webhook endpoint will be live and staging orders.
- **Each module is readable in isolation.** First-time Bicep — smaller files are easier to understand and debug.
- **Modules can be tested individually.** `az deployment group what-if` on a single module to preview changes.

## Module Details

### network.bicep

Creates the VNet and all subnets.

| Subnet | CIDR | Purpose |
|--------|------|---------|
| snet-container-apps | 172.28.1.0/23 | Container Apps Environment (min /23) |
| snet-postgres | 172.28.4.0/24 | PostgreSQL delegated subnet |
| GatewaySubnet | 172.28.255.0/27 | VPN Gateway (name is mandatory) |

**NSG rules on container apps subnet:**
- Allow outbound to 192.168.3.0/24 on port 31002 (e.net REST) — SYSPRO access
- Allow outbound to 172.28.4.0/24 on port 5432 — PostgreSQL
- Allow inbound on port 8080 from any (Shopify webhooks)
- Deny all other outbound to private IP ranges (defence in depth)

### postgres.bicep

- **Server**: rectella-db-prod (globally unique name, parameterised)
- **SKU**: Burstable B1ms — 1 vCore, 2GB RAM. Plenty for approx 40 SKUs and low order volume.
- **Storage**: 32GB (minimum, auto-grow enabled)
- **Version**: PostgreSQL 16
- **Access**: Private VNet integration via delegated subnet. No public access.
- **Database**: rectella (created by Bicep)
- **Admin user**: rectellaadmin (password passed as secure parameter)
- **Connection string format**: `postgres://rectellaadmin:<password>@<server>.postgres.database.azure.com:5432/rectella?sslmode=require`
- **Backups**: Default 7-day retention (Azure managed, no extra cost on Burstable)

### container-apps.bicep

- **Environment**: VNet-integrated into snet-container-apps, internal + external ingress
- **Container App**: rectella-shopify-svc
  - Image from ACR (parameterised tag)
  - 0.5 vCPU, 1Gi memory (sufficient for Go binary)
  - Min replicas: 1, Max replicas: 1 (single instance — batch processor assumes single-flight)
  - External ingress on port 8080 (auto-generated FQDN with TLS)
  - Health probes: liveness on /health, readiness on /ready
- **Environment variables** (set as Container App secrets + env var refs):
  - DATABASE_URL — constructed from Postgres outputs
  - SYSPRO_ENET_URL — http://192.168.3.150:31002/SYSPROWCFService/Rest
  - SYSPRO_OPERATOR, SYSPRO_PASSWORD, SYSPRO_COMPANY_ID
  - SHOPIFY_WEBHOOK_SECRET, SHOPIFY_ACCESS_TOKEN, SHOPIFY_STORE_URL
  - SHOPIFY_API_KEY, SHOPIFY_API_SECRET
  - ADMIN_TOKEN
  - SYSPRO_WAREHOUSE, SYSPRO_SKUS
  - PORT=8080
  - BATCH_INTERVAL, STOCK_SYNC_INTERVAL, LOG_LEVEL

**Note**: Min replicas = 1 (not scale-to-zero) because the batch processor and stock sync loops must always be running. Scale-to-zero would stop background processing when no HTTP requests are incoming.

### registry.bicep

- **Name**: rectellaacr (globally unique, alphanumeric only)
- **SKU**: Basic (500 GiB storage, 2 webhooks)
- **Admin access**: Enabled for simple docker login + Container Apps pull
- **Image naming**: rectellaacr.azurecr.io/rectella-shopify-svc:<tag>

### vpn.bicep

This module is **independently deployable** — omit it from main.bicep until NCS is ready.

- **Public IP**: pip-vpn-rectella (Standard SKU, static)
- **VPN Gateway**: vpng-rectella
  - SKU: VpnGw1
  - Type: RouteBased
  - VPN Type: IKEv2
  - Provisioning time: approx 25 to 40 minutes (Azure allocates dedicated hardware)
- **Local Network Gateway**: lng-rectella-meraki
  - gatewayIpAddress: Meraki public IP (parameter, from NCS)
  - addressPrefixes: 192.168.3.0/24
- **Connection**: conn-rectella-site2site
  - Type: IPsec
  - Shared key: parameter (agreed with NCS)
  - IPsec policy: IKEv2, AES256, SHA256 (Meraki compatible defaults)

**What NCS needs to configure on the Meraki:**
1. Site-to-site VPN peer: Azure VPN Gateway public IP (output from deployment)
2. Remote subnets: 172.28.0.0/16
3. Pre-shared key: agreed value
4. IPsec: IKEv2, AES256, SHA256

## Parameters

### Non-Secret Parameters (in prod.bicepparam)

```
location = 'uksouth'
environment = 'prod'
postgresServerName = 'rectella-db-prod'
postgresDbName = 'rectella'
postgresAdminUser = 'rectellaadmin'
registryName = 'rectellaacr'
containerAppName = 'rectella-shopify-svc'
containerImageTag = 'latest'
sysproPnetUrl = 'http://192.168.3.150:31002/SYSPROWCFService/Rest'
onPremAddressPrefixes = ['192.168.3.0/24']
batchInterval = '5m'
stockSyncInterval = '15m'
logLevel = 'info'
minReplicas = 1
maxReplicas = 1
deployVpn = false
```

### Secret Parameters (passed at deploy time via CLI)

- postgresAdminPassword — Strong random password
- sysproPOperator — SYSPRO operator name
- sysproPPassword — SYSPRO operator password (may be blank)
- sysproPCompanyId — Production company ID
- shopifyWebhookSecret — HMAC secret from Shopify app
- shopifyAccessToken — shpat token from Shopify custom app
- shopifyApiKey — Shopify app API key
- shopifyApiSecret — Shopify app secret
- shopifyStoreUrl — e.g. rectella.myshopify.com
- adminToken — Shared secret for /orders endpoints
- sysproPWarehouse — Warehouse code
- sysproPSkus — Comma-separated SKUs
- merakiPublicIp — From NCS (VPN module only)
- vpnSharedKey — Agreed with NCS (VPN module only)

All secrets stored as Container Apps secrets, referenced by env vars. Never in parameter files or source control.

## Deployment Workflow

### Prerequisites

1. Azure subscription created (Rectella account)
2. az CLI installed and authenticated
3. Bicep CLI installed (az bicep install)
4. Resource group created: `az group create -n rg-rectella-prod -l uksouth`

### First Deployment (everything except VPN)

```bash
# Validate first (no changes made)
az deployment group what-if \
  --resource-group rg-rectella-prod \
  --template-file infra/main.bicep \
  --parameters infra/parameters/prod.bicepparam \
  --parameters postgresAdminPassword='...' shopifyWebhookSecret='...'

# Deploy (approx 5 to 10 min without VPN)
az deployment group create \
  --resource-group rg-rectella-prod \
  --template-file infra/main.bicep \
  --parameters infra/parameters/prod.bicepparam \
  --parameters postgresAdminPassword='...' shopifyWebhookSecret='...'
```

### Push Container Image

```bash
# Build and push to ACR (simplest — builds in Azure, no local Docker needed)
az acr build --registry rectellaacr \
  --image rectella-shopify-svc:latest \
  --file Dockerfile .

# Or local build + push
docker build -t rectellaacr.azurecr.io/rectella-shopify-svc:latest .
az acr login --name rectellaacr
docker push rectellaacr.azurecr.io/rectella-shopify-svc:latest
```

### Update Running App

```bash
az containerapp update \
  --name rectella-shopify-svc \
  --resource-group rg-rectella-prod \
  --image rectellaacr.azurecr.io/rectella-shopify-svc:latest
```

### Add VPN Later (when NCS is ready)

Deploy with the VPN flag enabled and Meraki details:

```bash
az deployment group create \
  --resource-group rg-rectella-prod \
  --template-file infra/main.bicep \
  --parameters infra/parameters/prod.bicepparam \
  --parameters deployVpn=true merakiPublicIp='x.x.x.x' vpnSharedKey='...'
```

Then share the VPN Gateway public IP (from deployment output) with NCS for Meraki configuration.

### Verification After Deploy

```bash
# Check container is running
az containerapp show --name rectella-shopify-svc \
  -g rg-rectella-prod --query "properties.runningStatus"

# Get the auto-generated URL
az containerapp show --name rectella-shopify-svc \
  -g rg-rectella-prod --query "properties.configuration.ingress.fqdn" -o tsv

# Hit health endpoint
curl https://<fqdn>/health

# Check logs
az containerapp logs show --name rectella-shopify-svc \
  -g rg-rectella-prod --follow

# Check VPN tunnel status (after VPN deployment)
az network vpn-connection show -n conn-rectella-site2site \
  -g rg-rectella-prod --query "connectionStatus"
```

## Phased Deployment Strategy

This design supports deploying in phases, which de-risks go-live:

**Phase A — App + DB (no SYSPRO connectivity)**
Deploy Container Apps + PostgreSQL + ACR. Shopify webhooks will be received and staged in the database. Batch processor will run but fail to connect to SYSPRO (orders stay pending). This validates: webhook endpoint works, HMAC verification, database persistence, container health.

**Phase B — Add VPN**
Deploy VPN module when NCS is ready. Batch processor will now reach SYSPRO. Pending orders will be processed on next batch cycle.

**Phase C — Live orders**
Configure Shopify webhook to point at the Container App URL. Real orders flow through.

This means even if NCS is slow on VPN, the webhook endpoint can be live and staging orders. No orders are lost — they queue until SYSPRO connectivity is established.

## What This Spec Does NOT Cover

- **CI/CD pipeline**: Manual az acr build + az containerapp update is sufficient for go-live. GitHub Actions can be added during hypercare.
- **Custom domain**: Using auto-generated Azure FQDN. Add custom domain later if requested.
- **Monitoring/alerting**: Azure Log Analytics workspace and alerting rules are separate work (noted in go-live gaps).
- **Key Vault**: Secrets are stored as Container Apps secrets. Key Vault integration is an improvement for later.
- **Backup/DR**: Azure-managed PostgreSQL backups (7-day default). No cross-region DR for Phase 1.

## Open Questions

1. **VPN timing**: When will NCS be available to configure the Meraki side? This determines whether Phase A and B are separate deploys or one.
2. **Production SYSPRO company ID**: Still needed from Sarah — currently using RILT (test).
3. **PostgreSQL server name**: Must be globally unique. rectella-db-prod may be taken — parameterised for flexibility.
4. **ACR name**: Must be globally unique, alphanumeric only. rectellaacr may be taken.
