// Minimal test deployment for ctrlaltinsight Azure subscription.
// Public web app + public Postgres, no VNet, no VPN.
// Purpose: prove the App Service + Docker + Postgres deploy path works end-to-end
// before we redeploy onto Rectella's subscription with the VPN.

targetScope = 'resourceGroup'

param location string = resourceGroup().location
param containerImage string = 'ghcr.io/trismegistus0/rectella-shopify-service:sha-eba4ab5'

@secure()
param pgAdminPassword string

@secure()
param shopifyWebhookSecret string

@secure()
param shopifyAccessToken string

param shopifyStoreUrl string = 'h0snak-s5.myshopify.com'

@secure()
param adminToken string

// ---- PostgreSQL Flexible Server (public access for test) ----

resource pg 'Microsoft.DBforPostgreSQL/flexibleServers@2024-08-01' = {
  name: 'rectella-test-pg-${uniqueString(resourceGroup().id)}'
  location: location
  sku: { name: 'Standard_B1ms', tier: 'Burstable' }
  properties: {
    version: '16'
    administratorLogin: 'rectella'
    administratorLoginPassword: pgAdminPassword
    storage: { storageSizeGB: 32, autoGrow: 'Disabled' }
    backup: { backupRetentionDays: 7, geoRedundantBackup: 'Disabled' }
    network: { publicNetworkAccess: 'Enabled' }
    highAvailability: { mode: 'Disabled' }
  }
}

resource pgDb 'Microsoft.DBforPostgreSQL/flexibleServers/databases@2024-08-01' = {
  parent: pg
  name: 'rectella'
  properties: { charset: 'UTF8', collation: 'en_US.utf8' }
}

// Allow Azure services + open to internet for first-time test (will lock down later).
resource pgFwAzure 'Microsoft.DBforPostgreSQL/flexibleServers/firewallRules@2024-08-01' = {
  parent: pg
  name: 'AllowAllAzureServices'
  properties: { startIpAddress: '0.0.0.0', endIpAddress: '0.0.0.0' }
}

// ---- App Service Plan (B1 Linux) ----

resource plan 'Microsoft.Web/serverfarms@2024-04-01' = {
  name: 'rectella-test-plan'
  location: location
  sku: { name: 'B1', tier: 'Basic' }
  kind: 'linux'
  properties: { reserved: true }
}

// ---- Web App for Containers ----

resource webApp 'Microsoft.Web/sites@2024-04-01' = {
  name: 'rectella-test-${uniqueString(resourceGroup().id)}'
  location: location
  kind: 'app,linux,container'
  properties: {
    serverFarmId: plan.id
    httpsOnly: true
    siteConfig: {
      linuxFxVersion: 'DOCKER|${containerImage}'
      alwaysOn: true
      ftpsState: 'Disabled'
      http20Enabled: true
      minTlsVersion: '1.2'
      healthCheckPath: '/health'
      appSettings: [
        { name: 'WEBSITES_PORT', value: '8080' }
        { name: 'WEBSITES_ENABLE_APP_SERVICE_STORAGE', value: 'false' }
        { name: 'DOCKER_ENABLE_CI', value: 'false' }
        { name: 'PORT', value: '8080' }
        { name: 'LOG_LEVEL', value: 'info' }
        { name: 'DATABASE_URL', value: 'postgres://rectella:${pgAdminPassword}@${pg.properties.fullyQualifiedDomainName}:5432/rectella?sslmode=require' }
        { name: 'SHOPIFY_WEBHOOK_SECRET', value: shopifyWebhookSecret }
        { name: 'SHOPIFY_STORE_URL', value: shopifyStoreUrl }
        { name: 'SHOPIFY_ACCESS_TOKEN', value: shopifyAccessToken }
        // SYSPRO is unreachable from this sub (no VPN). Use a sentinel URL —
        // the service will boot, fail SYSPRO calls, but webhook ingestion works.
        { name: 'SYSPRO_ENET_URL', value: 'http://mocksyspro.invalid/SYSPROWCFService/Rest' }
        { name: 'SYSPRO_OPERATOR', value: 'test' }
        { name: 'SYSPRO_PASSWORD', value: '' }
        { name: 'SYSPRO_COMPANY_ID', value: 'TEST' }
        { name: 'SYSPRO_WAREHOUSE', value: 'WEBS' }
        { name: 'ADMIN_TOKEN', value: adminToken }
        { name: 'BATCH_INTERVAL', value: '5m' }
        { name: 'STOCK_SYNC_INTERVAL', value: '15m' }
        { name: 'FULFILMENT_SYNC_INTERVAL', value: '30m' }
        { name: 'RECONCILIATION_INTERVAL', value: '0' }
      ]
    }
  }
}

output webAppHostname string = webApp.properties.defaultHostName
output pgFqdn string = pg.properties.fullyQualifiedDomainName
output webhookUrl string = 'https://${webApp.properties.defaultHostName}/webhooks/orders/create'
