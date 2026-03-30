package main

import (
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type mockSyspro struct {
	port     int
	orderSeq atomic.Int64
	server   *http.Server
}

func newMockSyspro(port int) *mockSyspro {
	m := &mockSyspro{port: port}
	mux := http.NewServeMux()
	mux.HandleFunc("/SYSPROWCFService/Rest/Logon", m.handleLogon)
	mux.HandleFunc("/SYSPROWCFService/Rest/Transaction/Post", m.handleTransaction)
	mux.HandleFunc("/SYSPROWCFService/Rest/Query/Query", m.handleQuery)
	mux.HandleFunc("/SYSPROWCFService/Rest/Logoff", m.handleLogoff)
	m.server = &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	return m
}

func (m *mockSyspro) start() error {
	go m.server.ListenAndServe()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", m.port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("mock SYSPRO did not start on port %d within 2s", m.port)
}

func (m *mockSyspro) stop() { m.server.Close() }

func (m *mockSyspro) handleLogon(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "mock-session-001")
}

func (m *mockSyspro) handleTransaction(w http.ResponseWriter, r *http.Request) {
	seq := m.orderSeq.Add(1)
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="Windows-1252"?>
<SalesOrders>
  <Orders><OrderHeader>
    <SalesOrder>SO-MOCK-%03d</SalesOrder>
    <CustomerPoNumber></CustomerPoNumber>
  </OrderHeader></Orders>
  <ValidationStatus><Status>Successful</Status></ValidationStatus>
  <StatusOfItems><ItemsProcessed>1</ItemsProcessed><ItemsInvalid>0</ItemsInvalid></StatusOfItems>
</SalesOrders>`, seq)
}

func (m *mockSyspro) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprint(w, `<?xml version="1.0" encoding="Windows-1252"?>
<InvQuery><StockItem><StockCode>CBBQ0001</StockCode><AvailableQty>100.000</AvailableQty></StockItem></InvQuery>`)
}

func (m *mockSyspro) handleLogoff(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "true")
}
