# SYSPRO 8 SORTOI: Per-Line Tax Code Override

## Executive Summary

Per-line tax code override **is supported** in SORTOI via the `<StockTaxCode>` element on `<StockLine>`. However, it is gated by a Sales Order Setup option that must be enabled before the element takes effect. Without that prerequisite, the element is silently discarded — matching the behaviour observed in testing. Two additional related elements (`<StockNotTaxable>`, `<StockFstCode>`, `<StockNotFstTaxable>`) are also available on `<StockLine>` for taxability and GST/FST control.

The three element names already tested (`ProductTaxCode`, `TaxCode`, `MProductTaxCode`) are all confirmed non-existent in the SORTOI schema; the correct name is `StockTaxCode`.

***

## Confirmed StockLine Tax Elements

The following elements exist in the SORTOI `<StockLine>` input schema, confirmed from multiple independent sources including a complete SORTOI integration code sample and the CyberStore XSLT:[^1][^2]

| Element | Purpose | Notes |
|---|---|---|
| `<StockTaxCode>` | Override the VAT/tax code on the line | Requires Setup Option to be enabled |
| `<StockNotTaxable>` | Mark line as non-taxable (exempt) | `Y` = exempt; blank = use stock master |
| `<StockFstCode>` | Override the GST/FST tax code on the line | Australia/Canada GST; not relevant for UK VAT |
| `<StockNotFstTaxable>` | Mark line as non-FST-taxable | GST exemption flag |

The complete set was enumerated in a real SORTOI integration (VBA/ChilkatXML against SYSPRO e.net), where `StockTaxCode` and `StockNotTaxable` appear consecutively in the `<StockLine>` node. The CyberStore XSLT (which is built directly against the SORTOI XSD) uses `<StockNotTaxable>` and `<StockFstCode>` in its `<StockLine>` template.[^2][^1]

***

## Why StockTaxCode Was Silently Ignored

SORTOI honours `<StockTaxCode>` only if **"Allow changes to tax code for stocked items"** is enabled in Sales Order Setup. This is a Sales Order Setup option found on the **Tax/Um tab**.[^3][^4][^5]

When this option is disabled:
- SYSPRO's Sales Order Entry UI does not allow operators to change the tax code on stocked lines
- SORTOI mirrors this constraint: the element is parsed but the value is discarded
- No error is raised; the import succeeds with the stock-master tax code applied

This explains exactly why `<StockTaxCode>E</StockTaxCode>` (and the other guesses) were silently dropped — the system recognised the element but the override was blocked at the business rule level.

There is a related option **"Allow changes to tax and GST codes for stocked items"** that extends this to cover both tax code and GST code. For UK VAT only, the base "tax code" option is sufficient.[^5]

***

## Required Setup Steps

### Step 1: Enable the Sales Order Setup option

**Path:** SYSPRO Ribbon bar → Setup → Distribution → Sales Order Setup → **Tax/Um tab**

Enable: **Allow changes to tax code for stocked items**

This must be done by a SYSPRO administrator (Sarah / whoever manages Sales Order Setup). It is a company-wide setting and affects all operators using Sales Order Entry as well as SORTOI.

> ⚠️ Note: CyberStore's documentation explicitly states this option must be enabled in SYSPRO's "Sales Order Setup → Tax/Um tab → Tax options" before the line-item tax override feature works in any e-commerce import context.[^4]

### Step 2: Use the correct element in StockLine

```xml
<StockLine>
  <CustomerPoLine>1</CustomerPoLine>
  <LineActionType>A</LineActionType>
  <StockCode>CHARCOAL-5KG</StockCode>
  <Warehouse>WH1</Warehouse>
  <OrderQty>2</OrderQty>
  <OrderUom>EA</OrderUom>
  <Price>12.99</Price>
  <PriceUom>EA</PriceUom>
  <StockTaxCode>R</StockTaxCode>
</StockLine>
```

Where the value (`R`, `E`, etc.) must match a valid tax code defined in SYSPRO's Tax Code Setup that carries the 5% rate.

***

## No Parameters-Level Tax Override

The SORTOI `<Parameters>` block does **not** contain a tax override element. Confirmed Parameters elements are limited to processing behaviour controls:[^6]

- `InBoxMsgReqd`
- `Process`
- `WarehouseListToUse`
- `AcceptEarlierShipDate`
- `AllocationAction`
- `AddStockSalesOrderText` / `AddDangerousGoodsText`
- `UseStockDescSupplied`
- `CreditFailMessage`
- `AllowDuplicateOrderNumbers`
- `AlwaysUsePriceEntered`
- `AllowZeroPrice`
- `ShipFromDefaultBin`
- `IgnoreWarnings`
- `OrderStatus`
- `TypeOfOrder`

There is no `TaxCode`, `OverrideTax`, `DefaultTaxCode`, or similar element at the Parameters level. Tax code control is line-scoped only, via `<StockTaxCode>` on `<StockLine>`.

***

## Definitive Source Confirmation

The canonical source is the SORTOI.XSD / SORTOIDOC.XSD schema files installed at `<SYSPRO install>\Base\Schemas\`. These XSD files are the ground truth; all element names above are consistent with multiple independent third-party implementations (CyberStore XSLT, handheld.ie integration code, Jonssonworkwear SOAP service) that were built against those XSD files.[^7][^8][^9][^1][^2]

SYSPRO's own support documentation confirms that SORTOIDOC.XML is the sample Document XML for SORTOI input, and SORTOIDOC.XSD is its schema.[^9]

***

## UK VAT Context Notes

For the Shopify → SYSPRO use case (5% domestic fuel VAT vs. 20% standard):

- `<StockNotTaxable>Y</StockNotTaxable>` marks a line as fully exempt — this is **not** what is needed for 5% reduced rate
- `<StockTaxCode>X</StockTaxCode>` where `X` is the 5%-rate tax code is the correct approach
- The tax code value must match a code configured in SYSPRO's Tax Code Setup with a 5% rate
- Sarah's concern about "cannot change per-warehouse" relates to the stock master setup, not SORTOI — SORTOI's `<StockTaxCode>` overrides at the order-line level regardless of the stock master

The SYSPRO Setup Options Tax page confirms the basic tax system uses the tax code held against the stock item as the default, but this can be overridden at the line level when the Sales Order Setup option is enabled.[^10][^11]

---

## References

1. [SORTOI](https://documentation.cyberstoreforsyspro.com/oasis2.21/SORTOI.html) - See the example of SORTOI code below: SORTOI.xslt. <?xml version="1.0" encoding="utf-8" ?> <xsl:styl...

2. [Posting Sales Orders in Syspro using XML and Business Objects](https://handheld.ie/posting-sales-orders-in-syspro-part/) - This post covers the export of the data from the validated database into Syspro. To use this code yo...

3. [SYSPRO USA AVP Tax System Requirements When Using ...](https://documentation.cyberstoreforsyspro.com/epayment2023/Line-Item-Taxability-AVP%20Settings.html) - CyberStore allows you to set line Item taxability. However, in order for this function to work prope...

4. [SYSPRO USA AVP Tax System Requirements When Using CyberStore Line Item Taxability Override](https://documentation.cyberstoreforsyspro.com/ecommerce2023/Line-Item-Taxability-AVP%20Settings.html) - CyberStore allows you to set line Item taxability. However, in order for this function to work prope...

5. [Sales Order Setup - SYSPRO Help](https://help.syspro.com/syspro-7-update-1/impord.htm)

6. [SORTOI Parameters - CyberStore Ecommerce 2023 Documentation](https://documentation.cyberstoreforsyspro.com/oasis2.21/SORTOI-Params.html) - See the example of SORTOI-Params code below:

7. [How to use XSD2Code for SYSPRO's Business Objects - Phil Detail](http://phildetail.blogspot.com/2014/04/how-to-use-xsd2code-for-syspros.html) - Set the Code/NameSpace to <your-namespace> + the XSD name, so that there aren't clashes with other X...

8. [CreateSalesOrder - syspro](https://publicapi.jonssonworkwear.com/syspro.asmx?op=CreateSalesOrder)

9. [The sample XML and Schema files provided with SYSPRO](https://support.syspro.com/support/solutions/articles/77000498710-the-sample-xml-and-schema-files-provided-with-syspro) - Sample XML files SYSPRO ships with sample XML files for most business objects in the Base\Schemas fo...

10. [Sales Orders Tax - SYSPRO Help](https://help.syspro.com/syspro-8-2023/g_programs/imp/impcfg/forms/tax/impcfgtd.htm?TocPath=ADMINISTRATION%7CConfiguration%7CSetup+Options%7CTax%7C_____8) - This uses the tax code held against an item when processing credit notes for stocked lines and the t...

11. [Tax - SYSPRO Help](https://help.syspro.com/syspro-8-2018-r2/g_programs/imp/impcfg/forms/tax/impcfg-tax.htm)

