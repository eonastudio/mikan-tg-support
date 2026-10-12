package store

import (
	"context"
	"encoding/json"
	"time"
)

// Payment is one payment as the panel told it (payment.paid / payment.refunded).
type Payment struct {
	At       int64  `json:"at"`
	ID       int64  `json:"payment_id,omitempty"`
	Kind     string `json:"kind,omitempty"`     // new | renew | package | trial
	Provider string `json:"provider,omitempty"` // stars | addon:<id> | ...
	Amount   int64  `json:"amount"`             // minor units
	Currency string `json:"currency,omitempty"`
	TermDays int64  `json:"term_days,omitempty"`
	Refunded bool   `json:"refunded,omitempty"`
}

// Profile is what the panel's events told of one Telegram user: the personalization
// the AI answers and the agent sees on the ticket page. It only ever grows from the
// feed (no admin writes it by hand).
type Profile struct {
	TgID       int64     `json:"tg_id"`
	UserID     int64     `json:"user_id,omitempty"`
	ExpiresAt  int64     `json:"expires_at,omitempty"`  // unix, from user.expiring / user.expired
	DaysLeft   int       `json:"days_left,omitempty"`   // 3 or 1, from user.expiring
	Expired    bool      `json:"expired,omitempty"`     // from user.expired
	Traffic90  int64     `json:"traffic_90,omitempty"`  // unix, the first 90 % notice
	Traffic100 int64     `json:"traffic_100,omitempty"` // unix, the 100 % notice
	PeriodFrom int64     `json:"period_from,omitempty"` // the traffic period those are about
	Payments   []Payment `json:"payments,omitempty"`    // newest first, capped
	UpdatedAt  int64     `json:"updated_at"`
}

const (
	maxPayments = 10
	profileTTL  = 180 * 24 * time.Hour // an untouched profile goes away
)

// Profile reads a user's profile; a user the feed never spoke of is ErrNotFound.
func (s *Store) Profile(ctx context.Context, tgID int64) (Profile, error) {
	var p Profile
	if _, err := getJSON(ctx, s, profileKey(tgID), &p); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// touchProfile is one feed event applied under CAS: fn mutates the draft. Every write
// renews the TTL: a user the feed stops speaking of leaves the store after half a year.
func (s *Store) touchProfile(ctx context.Context, tgID int64, fn func(p *Profile)) error {
	return casJSONTTL(ctx, s, profileKey(tgID), profileTTL, func(cur *Profile) (*Profile, error) {
		p := Profile{TgID: tgID}
		if cur != nil {
			p = *cur
		}
		fn(&p)
		p.UpdatedAt = s.now().Unix()
		return &p, nil
	})
}

// ApplyEvent folds one panel event into the profile of its tg_id. data is the raw
// event JSON; the caller has checked the type. No tg_id: nothing to fold into.
func (s *Store) ApplyEvent(ctx context.Context, tgID, userID int64, typ string, at time.Time, data json.RawMessage) error {
	if tgID == 0 {
		return nil
	}
	// Shape the event per type first: a malformed payload keeps the profile, just not
	// the bad field.
	switch typ {
	case "payment.paid":
		var e struct {
			PaymentID int64  `json:"payment_id"`
			Kind      string `json:"kind"`
			Provider  string `json:"provider"`
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
			TermDays  int64  `json:"term_days"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil
		}
		return s.touchProfile(ctx, tgID, func(p *Profile) {
			p.UserID = firstNonZero(p.UserID, userID)
			p.Payments = append([]Payment{{At: at.Unix(), ID: e.PaymentID, Kind: e.Kind, Provider: e.Provider,
				Amount: e.Amount, Currency: e.Currency, TermDays: e.TermDays}}, p.Payments...)
			if len(p.Payments) > maxPayments {
				p.Payments = p.Payments[:maxPayments]
			}
		})
	case "payment.refunded":
		var e struct {
			PaymentID int64 `json:"payment_id"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil
		}
		return s.touchProfile(ctx, tgID, func(p *Profile) {
			p.UserID = firstNonZero(p.UserID, userID)
			for i := range p.Payments {
				if p.Payments[i].ID == e.PaymentID && e.PaymentID != 0 {
					p.Payments[i].Refunded = true
					break
				}
			}
		})
	case "user.expiring":
		var e struct {
			Days      int   `json:"days"`
			ExpiresAt int64 `json:"expires_at"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil
		}
		return s.touchProfile(ctx, tgID, func(p *Profile) {
			p.UserID = firstNonZero(p.UserID, userID)
			p.ExpiresAt = e.ExpiresAt
			p.DaysLeft = e.Days
			p.Expired = false
		})
	case "user.expired":
		var e struct {
			ExpiresAt int64 `json:"expires_at"`
		}
		_ = json.Unmarshal(data, &e) // absence of the field is fine
		return s.touchProfile(ctx, tgID, func(p *Profile) {
			p.UserID = firstNonZero(p.UserID, userID)
			p.Expired = true
			p.DaysLeft = 0
			if e.ExpiresAt > 0 {
				p.ExpiresAt = e.ExpiresAt
			}
		})
	case "traffic.90", "traffic.100":
		var e struct {
			PeriodFrom int64 `json:"period_from"`
		}
		_ = json.Unmarshal(data, &e)
		return s.touchProfile(ctx, tgID, func(p *Profile) {
			p.UserID = firstNonZero(p.UserID, userID)
			if typ == "traffic.90" {
				p.Traffic90 = at.Unix()
			} else {
				p.Traffic100 = at.Unix()
			}
			if e.PeriodFrom > 0 {
				p.PeriodFrom = e.PeriodFrom
			}
		})
	}
	return nil
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}
