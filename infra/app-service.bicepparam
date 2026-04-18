using './app-service.bicep'

param vnetName = 'Rectella-Network'
param appServiceSubnetPrefix = '10.0.6.0/27'

// Pin to current head commit for reproducibility.
param containerImage = 'ghcr.io/trismegistus0/rectella-shopify-service:sha-eba4ab5'

param shopifyStoreUrl = 'h0snak-s5.myshopify.com'
param sysproEnetUrl = 'http://192.168.3.150:31002/SYSPROWCFService/Rest'
param sysproOperator = 'ctrlaltinsight'

// Secrets populated from env vars exported by deploy-app-service.sh
param databaseUrl = readEnvironmentVariable('DATABASE_URL')
param shopifyWebhookSecret = readEnvironmentVariable('SHOPIFY_WEBHOOK_SECRET', '')
param shopifyAccessToken = readEnvironmentVariable('SHOPIFY_ACCESS_TOKEN', '')
param shopifyLocationId = readEnvironmentVariable('SHOPIFY_LOCATION_ID', '')
param sysproPassword = readEnvironmentVariable('SYSPRO_PASSWORD', '')
param sysproCompanyId = readEnvironmentVariable('SYSPRO_COMPANY_ID', 'RIL')
param sysproCompanyPassword = readEnvironmentVariable('SYSPRO_COMPANY_PASSWORD', '')
param sysproWarehouse = readEnvironmentVariable('SYSPRO_WAREHOUSE', '')
param sysproSkus = readEnvironmentVariable('SYSPRO_SKUS', '')
param adminToken = readEnvironmentVariable('ADMIN_TOKEN', '')

param logLevel = 'info'
