package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type mockSyspro struct {
	port     int
	orderSeq atomic.Int64
	server   *http.Server

	// Track submitted orders so SORQRY can return status "9" for them.
	mu              sync.Mutex
	submittedOrders map[string]string // syspro order number -> "submitted"
}

func newMockSyspro(port int) *mockSyspro {
	m := &mockSyspro{
		port:            port,
		submittedOrders: make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/SYSPROWCFService/Rest/Logon", m.handleLogon)
	mux.HandleFunc("/SYSPROWCFService/Rest/Transaction/Post", m.handleTransaction)
	mux.HandleFunc("/SYSPROWCFService/Rest/Query/Query", m.handleQuery)
	mux.HandleFunc("/SYSPROWCFService/Rest/Logoff", m.handleLogoff)
	m.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return m
}

func (m *mockSyspro) start() error {
	go func() { _ = m.server.ListenAndServe() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", m.port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("mock SYSPRO did not start on port %d within 2s", m.port)
}

func (m *mockSyspro) stop() { _ = m.server.Close() }

func (m *mockSyspro) handleLogon(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, "mock-session-001")
}

func (m *mockSyspro) handleTransaction(w http.ResponseWriter, r *http.Request) {
	seq := m.orderSeq.Add(1)
	orderNum := fmt.Sprintf("SO-MOCK-%03d", seq)

	// Track submitted order for SORQRY.
	m.mu.Lock()
	m.submittedOrders[orderNum] = "submitted"
	m.mu.Unlock()

	// Read and discard body to avoid connection issues.
	_, _ = io.ReadAll(r.Body)

	w.Header().Set("Content-Type", "text/xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="Windows-1252"?>
<SalesOrders>
  <Orders><OrderHeader>
    <SalesOrder>%s</SalesOrder>
    <CustomerPoNumber></CustomerPoNumber>
  </OrderHeader></Orders>
  <ValidationStatus><Status>Successful</Status></ValidationStatus>
  <StatusOfItems><ItemsProcessed>1</ItemsProcessed><ItemsInvalid>0</ItemsInvalid></StatusOfItems>
</SalesOrders>`, orderNum)
}

func (m *mockSyspro) handleQuery(w http.ResponseWriter, r *http.Request) {
	bo := r.URL.Query().Get("BusinessObject")
	w.Header().Set("Content-Type", "text/xml")

	switch bo {
	case "SORQRY":
		// Extract order number from XmlIn.
		xmlIn := r.URL.Query().Get("XmlIn")
		orderNum := extractXMLValue(xmlIn, "SalesOrder")

		m.mu.Lock()
		_, found := m.submittedOrders[orderNum]
		m.mu.Unlock()

		if found {
			// Return status "9" (complete) for submitted orders.
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="Windows-1252"?>
<SorDetail>
  <SalesOrder>%s</SalesOrder>
  <OrderStatus>9</OrderStatus>
  <OrderStatusDesc>Complete</OrderStatusDesc>
  <ShippingInstrs>MockCarrier</ShippingInstrs>
  <ShippingInstrsCod>MCR</ShippingInstrsCod>
  <LastInvoice>MOCK-INV-001</LastInvoice>
</SorDetail>`, orderNum)
		} else {
			// Unknown order — return status "1" (open).
			_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="Windows-1252"?>
<SorDetail>
  <SalesOrder>%s</SalesOrder>
  <OrderStatus>1</OrderStatus>
  <OrderStatusDesc>Open</OrderStatusDesc>
  <ShippingInstrs></ShippingInstrs>
</SorDetail>`, orderNum)
		}

	default:
		// INVQRY — extract stock code from XmlIn to return per-SKU data.
		xmlIn := r.URL.Query().Get("XmlIn")
		sku := extractXMLValue(xmlIn, "StockCode")
		if sku == "" {
			sku = "UNKNOWN"
		}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="Windows-1252"?>
<InvQuery>
  <QueryOptions>
    <StockCode>%s</StockCode>
    <Description>Mock stock item</Description>
  </QueryOptions>
  <WarehouseItem>
    <Warehouse>WH01</Warehouse>
    <QtyOnHand>150.000</QtyOnHand>
    <AvailableQty>100.000</AvailableQty>
  </WarehouseItem>
</InvQuery>`, sku)
	}
}

func (m *mockSyspro) handleLogoff(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, "true")
}

// extractXMLValue is a quick-and-dirty XML value extractor for mock use.
func extractXMLValue(xml, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(xml, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(xml[start:], close)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(xml[start : start+end])
}
