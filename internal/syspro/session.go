package syspro

import (
	"context"
	"fmt"

	"github.com/trismegistus0/rectella-shopify-service/internal/model"
)

// enetSession is an open SYSPRO e.net session backed by a session GUID.
type enetSession struct {
	client *EnetClient
	guid   string
}

// OpenSession logs on to SYSPRO and returns a Session for submitting multiple
// orders. The caller MUST call Close when done — it releases the session mutex
// that prevents concurrent SYSPRO callers from evicting this session.
func (c *EnetClient) OpenSession(ctx context.Context) (Session, error) {
	c.sessionMu.Lock()

	guid, err := c.logon(ctx)
	if err != nil {
		c.sessionMu.Unlock()
		return nil, fmt.Errorf("syspro logon: %w", err)
	}
	return &enetSession{client: c, guid: guid}, nil
}

// SubmitOrder sends a single SORTOI transaction on this session.
func (s *enetSession) SubmitOrder(ctx context.Context, order model.Order, lines []model.OrderLine) (*SalesOrderResult, error) {
	paramsXML, dataXML, err := buildSORTOI(order, lines)
	if err != nil {
		return nil, fmt.Errorf("building SORTOI XML: %w", err)
	}

	s.client.logger.Debug("submitting SORTOI",
		"order_number", order.OrderNumber,
		"lines", len(lines),
	)

	respXML, err := s.client.transaction(ctx, s.guid, "SORTOI", paramsXML, dataXML)
	if err != nil {
		return nil, fmt.Errorf("syspro SORTOI transaction: %w", err)
	}

	return parseSORTOIResponse(respXML)
}

// Close logs off from SYSPRO and releases the session mutex.
// Always call this when done with the session.
func (s *enetSession) Close(ctx context.Context) error {
	defer s.client.sessionMu.Unlock()

	if err := s.client.logoff(ctx, s.guid); err != nil {
		s.client.logger.Warn("syspro logoff failed", "error", err)
		return err
	}
	return nil
}
