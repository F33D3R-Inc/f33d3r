package handler

// commerce_events.go — Themis's commerce decisions, applied to this brain.
//
// In Facet Architecture order:
//
//	triggering event  a commerce.events record (internal/sitra/commerce.go)
//	server handler    ApplyCommerceEvent
//	Facet renderer    none for plans and subscriptions: the projection rows are
//	                  what the next render of a gated work, a shop or a profile
//	                  reads. A delivered purchase raises the buyer's library
//	                  refresh.
//	stream mutation   marketplace_purchase_delivered on the buyer's FA Live
//	                  connections, on which the library Facet re-requests itself

import (
	"context"
	"errors"
	"fmt"

	dbpkg "github.com/f33d3r/feed-engine/internal/db"
	"github.com/f33d3r/feed-engine/internal/sitra"
)

// ApplyCommerceEvent carries out one commerce.events record.
func (h *Handler) ApplyCommerceEvent(ctx context.Context, ev sitra.CommerceEvent) error {
	if h.db == nil {
		return errors.New("commerce event: no database")
	}
	var err error
	switch ev.Type {
	case sitra.CommercePlanChanged:
		p := ev.Plan
		err = dbpkg.ApplyCommercePlan(ctx, h.db, ev.Seq, ev.EventID, dbpkg.CommercePlanState{
			ID:             p.ID,
			CreatorAccount: p.CreatorAccount,
			Name:           p.Name,
			Description:    p.Description,
			PriceUnits:     p.PriceUnits,
			IsActive:       p.IsActive,
		})
	case sitra.CommercePurchaseDelivered:
		fresh, claimErr := dbpkg.ClaimCommerceEvent(ctx, h.db, ev.Subject.Kind, ev.Subject.ID, ev.Seq, ev.EventID)
		if claimErr != nil {
			return claimErr
		}
		if fresh {
			// The buyer's library and any open purchase status line re-request
			// themselves; the key they need is rendered server-side from Themis.
			PublishToUser(ev.Purchase.BuyerPIAL, SSEEvent{Type: "marketplace_purchase_delivered", Data: ev.Purchase.ID})
		}
		return nil
	default:
		s := ev.Subscription
		planID := ""
		if s.PlanID != nil {
			planID = *s.PlanID
		}
		err = dbpkg.ApplyCommerceSubscription(ctx, h.db, ev.Subject.ID, ev.Seq, ev.EventID, dbpkg.CommerceSubscriptionState{
			ID:                s.ID,
			SubscriberAccount: s.SubscriberAccount,
			CreatorAccount:    s.CreatorAccount,
			PlanID:            planID,
			Status:            s.Status,
			PeriodEnd:         s.PeriodEnd,
			CancelledAt:       s.CancelledAt,
			PriceUnits:        s.PriceUnits,
		})
	}
	if errors.Is(err, dbpkg.ErrCommerceUnapplicable) {
		return fmt.Errorf("%w: %v", sitra.ErrPermanent, err)
	}
	return err
}

// CommerceDeadLetter keeps a commerce.events record that can never be applied.
func (h *Handler) CommerceDeadLetter(ctx context.Context, topic string, partition int32, offset int64, record []byte, reason string) error {
	return dbpkg.RecordCommerceDeadLetter(ctx, h.db, topic, partition, offset, record, reason)
}
