package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNotFound is returned by the VetclinicClient when a resource is not found (404).
var ErrNotFound = errors.New("vetclinic: not found")

// Subscription mirrors domain.Subscription from vetclinic-api (no shared package).
type Subscription struct {
	OwnerUID             string     `json:"owner_uid"`
	BillingStatus        string     `json:"billing_status"`
	DiscountTier         string     `json:"discount_tier"`
	BetaParticipant      bool       `json:"beta_participant"`
	MPSubscriptionID     string     `json:"mp_subscription_id,omitempty"`
	PlanID               string     `json:"plan_id,omitempty"`
	CurrentPeriodEnd     *time.Time `json:"current_period_end,omitempty"`
	LastProcessedEventID string     `json:"last_processed_event_id,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type Clinic struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OwnerUID      string `json:"owner_uid"`
	BillingStatus string `json:"billing_status"`
}

type VetclinicClient struct {
	baseURL string
	secret  string
	http    *http.Client
}

func NewVetclinicClient(baseURL, secret string) *VetclinicClient {
	return &VetclinicClient{
		baseURL: baseURL,
		secret:  secret,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// do executes an HTTP request to vetclinic-api. A 404 response returns ErrNotFound;
// callers that expect optional resources should handle it explicitly.
func (c *VetclinicClient) do(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var bodyBytes []byte
	var err error
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("X-Internal-Secret", c.secret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return ErrNotFound
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("vetclinic-api returned %d for %s %s", resp.StatusCode, method, path)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// GetSubscription fetches a subscription by ownerUID. Returns nil, nil when no subscription
// document exists yet (404), which is expected for owners who onboarded before billing.
func (c *VetclinicClient) GetSubscription(ctx context.Context, ownerUID string) (*Subscription, error) {
	var sub Subscription
	if err := c.do(ctx, http.MethodGet, "/internal/billing/subscriptions/"+ownerUID, nil, &sub); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if sub.OwnerUID == "" {
		return nil, nil
	}
	return &sub, nil
}

func (c *VetclinicClient) UpdateSubscription(ctx context.Context, ownerUID string, fields map[string]interface{}) error {
	return c.do(ctx, http.MethodPut, "/internal/billing/subscriptions/"+ownerUID, fields, nil)
}

func (c *VetclinicClient) GetClinicsByOwnerUID(ctx context.Context, ownerUID string) ([]Clinic, error) {
	var clinics []Clinic
	if err := c.do(ctx, http.MethodGet, "/internal/billing/clinics?owner_uid="+ownerUID, nil, &clinics); err != nil {
		return nil, err
	}
	return clinics, nil
}

func (c *VetclinicClient) UpdateClinicBillingStatus(ctx context.Context, clinicID, status string) error {
	return c.do(ctx, http.MethodPut, "/internal/billing/clinics/"+clinicID+"/billing-status",
		map[string]string{"billing_status": status}, nil)
}

func (c *VetclinicClient) QuerySubscriptions(ctx context.Context, discountTier, billingStatus string) ([]Subscription, error) {
	var subs []Subscription
	path := fmt.Sprintf("/internal/billing/subscriptions?discount_tier=%s&billing_status=%s", discountTier, billingStatus)
	if err := c.do(ctx, http.MethodGet, path, nil, &subs); err != nil {
		return nil, err
	}
	return subs, nil
}
