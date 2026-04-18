// App Service Linux B1 deployment — alternative to Container Apps.
// Supports on-prem VPN egress via Regional VNet Integration, which
// Container Apps Consumption profile does not.

targetScope = 'resourceGroup'

@description('Azure region')
param location string = resourceGroup().location

@description('Existing VNet name')
param vnetName string = 'Rectella-Network'

@description('CIDR for the new App Service delegated subnet (/27 minimum)')
param appServiceSubnetPrefix string = '10.0.6.0/27'

@description('Container image reference (prefer sha tag for reproducibility)')
param containerImage string = 'ghcr.io/trismegistus0/rectella-shopify-service:latest'

// Secrets passed from deploy script (currently live in the Container App).
@secure()
param databaseUrl string

@secure()
param shopifyWebhookSecret string

param shopifyStoreUrl string = 'rectella.myshopify.com'

@secure()
param shopifyAccessToken string

param shopifyLocationId string = ''

param sysproEnetUrl string = 'http://192.168.3.150:31002/SYSPROWCFService/Rest'

param sysproOperator string

@secure()
param sysproPassword string

param sysproCompanyId string

@secure()
param sysproCompanyPassword string = ''

param sysproWarehouse string = ''

param sysproSkus string = ''

@secure()
param adminToken string

param logLevel string = 'info'

// ---- Existing VNet ----

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' existing = {
  name: vnetName
}

// ---- App Service delegated subnet ----

resource appServiceSubnet 'Microsoft.Network/virtualNetworks/subnets@2024-05-01' = {
  parent: vnet
  name: 'app-service-subnet'
  properties: {
    addressPrefix: appServiceSubnetPrefix
    delegations: [
      {
        name: 'app-service-delegation'
        properties: {
          serviceName: 'Microsoft.Web/serverFarms'
        }
      }
    ]
  }
}

// ---- App Service Plan (Linux B1) ----

resource plan 'Microsoft.Web/serverfarms@2024-04-01' = {
  name: 'rectella-plan'
  location: location
  sku: {
    name: 'S1'
    tier: 'Standard'
  }
  kind: 'linux'
  properties: {
    reserved: true
  }
}

// ---- Web App for Containers ----

resource webApp 'Microsoft.Web/sites@2024-04-01' = {
  name: 'rectella-shopify-service'
  location: location
  kind: 'app,linux,container'
  properties: {
    serverFarmId: plan.id
    httpsOnly: true
    virtualNetworkSubnetId: appServiceSubnet.id
    vnetRouteAllEnabled: true
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
        { name: 'LOG_LEVEL', value: logLevel }
        { name: 'DATABASE_URL', value: databaseUrl }
        { name: 'SHOPIFY_WEBHOOK_SECRET', value: shopifyWebhookSecret }
        { name: 'SHOPIFY_STORE_URL', value: shopifyStoreUrl }
        { name: 'SHOPIFY_ACCESS_TOKEN', value: shopifyAccessToken }
        { name: 'SHOPIFY_LOCATION_ID', value: shopifyLocationId }
        { name: 'SYSPRO_ENET_URL', value: sysproEnetUrl }
        { name: 'SYSPRO_OPERATOR', value: sysproOperator }
        { name: 'SYSPRO_PASSWORD', value: sysproPassword }
        { name: 'SYSPRO_COMPANY_ID', value: sysproCompanyId }
        { name: 'SYSPRO_COMPANY_PASSWORD', value: sysproCompanyPassword }
        { name: 'SYSPRO_WAREHOUSE', value: sysproWarehouse }
        { name: 'SYSPRO_SKUS', value: sysproSkus }
        { name: 'ADMIN_TOKEN', value: adminToken }
        { name: 'BATCH_INTERVAL', value: '5m' }
        { name: 'STOCK_SYNC_INTERVAL', value: '15m' }
        { name: 'FULFILMENT_SYNC_INTERVAL', value: '30m' }
        // Reconciliation sweep is the recovery path for orders that fail
        // initial webhook delivery or start unpaid-then-paid. Load-bearing
        // for launch safety. Do NOT leave this unset.
        { name: 'RECONCILIATION_INTERVAL', value: '15m' }
      ]
    }
  }
}

// ---- Outputs ----

output webAppHostname string = webApp.properties.defaultHostName
output webhookOrdersCreateUrl string = 'https://${webApp.properties.defaultHostName}/webhooks/orders/create'
output webhookOrdersCancelledUrl string = 'https://${webApp.properties.defaultHostName}/webhooks/orders/cancelled'
