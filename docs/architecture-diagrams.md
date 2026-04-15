# Architecture Diagrams

Durable ASCII copy of the four pipeline diagrams. Interactive browser
version lives at `docs/architecture-playground.html`.

## 1. Big picture

```
  +----------------+         +----------------------+
  |    Customer    |  pays   |    Shopify Store     |
  |    (browser)   |-------->|      (cloud)         |
  +----------------+         +----------+-----------+
                                        |
                          orders/create |  ^ stock + fulfilment
                             webhook    |  | (GraphQL)
                                        v  |
                             +-------------+-----------+
                             |     Go Service          |
                             |  Azure App Service      |
                             |   VNet 10.0.6.0/27      |
                             +------+-----------+------+
                                    |           |
                         orders SQL |           | SORTOI / INVQRY
                                    v           | SORQRY over VPN
                          +------------------+  v
                          |    Postgres      | +---------------------+
                          |  10.0.2.0/24     | |  Azure VPN Gateway  |
                          +------------------+ +----------+----------+
                                                          |
                                                          v
                                                +--------------------+
                                                |  Rectella Meraki   |
                                                |  192.168.3.0/24    |
                                                +----------+---------+
                                                           v
                                                +----------------------+
                                                |  SYSPRO 8 (RIL-APP01)|
                                                |  192.168.3.150:31002 |
                                                +----------------------+
```

## 2. Order IN (webhook → SORTOI sales order)

```
  +------------------------------------+
  |  Customer pays on Shopify          |
  +------------------+-----------------+
                     | orders/create webhook
                     v
  +------------------------------------+
  |  Go service: HMAC verify + dedupe  |
  +------------------+-----------------+
                     | valid, new
                     v
  +------------------------------------+
  |  Postgres: INSERT order            |
  |  status = pending                  |
  +------------------+-----------------+
                     | every 5 minutes
                     v
  +------------------------------------+
  |  Batch processor picks pending     |
  +------------------+-----------------+
                     | SYSPRO /Logon
                     v
  +------------------------------------+
  |  Submit SORTOI XML -> SYSPRO       |
  |  receives real order number        |
  +------------------+-----------------+
                     | UPDATE
                     v
  +------------------------------------+
  |  Postgres: status = submitted      |
  |  syspro_order_number = 015575      |
  +------------------------------------+
```

## 3. Stock sync OUT (INVQRY → order-aware math → Shopify)

```
  +-------------+     SKU list      +-------------+    per SKU     +-------------+
  |   Shopify   |------------------>|  Go service |--------------->|   SYSPRO    |
  | GraphQL API |    productVariants|   (15 min)  |  INVQRY XML    | RIL-APP01   |
  +-------------+                   +------+------+                +------+------+
                                           |                              |
                                           |        QtyAvailable          |
                                           |<-----------------------------+
                                           v
                                    +--------------+
                                    |   Postgres   |  pending+processing
                                    |  order math  |  order quantities
                                    +------+-------+
                                           |
                                           | subtract reserved
                                           v
                                    +--------------+
                                    |  clamp to 0  |
                                    +------+-------+
                                           |
                                           | one batched mutation
                                           v
  +-------------+   inventorySetQuantities  +-------------+
  |   Shopify   |<--------------------------|  Go service |
  | storefront  |    (all SKUs at once)     |             |
  +-------------+                           +-------------+
```

## 4. Fulfilment BACK (SORQRY status 9 → fulfillmentCreate)

```
  +------------------------------------+
  |  Every 30 minutes                  |
  +------------------+-----------------+
                     | SELECT
                     v
  +------------------------------------+
  |  Postgres: orders status=submitted |
  +------------------+-----------------+
                     | for each order
                     v
  +------------------------------------+
  |  SYSPRO SORQRY (by order number)   |
  +------------------+-----------------+
                     | status = "9"
                     v
  +------------------------------------+
  |  Read ShippingInstrs               |
  |  carrier + tracking number         |
  +------------------+-----------------+
                     | fulfillmentCreate
                     v
  +------------------------------------+
  |  Shopify GraphQL: create fulfilment|
  +------------------+-----------------+
                     | UPDATE
                     v
  +------------------------------------+
  |  Postgres: status = fulfilled      |
  +------------------------------------+
```
