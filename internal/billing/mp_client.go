package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const mpBaseURL = "https://api.mercadopago.com"

type MPSubscriptionRequest struct {
	PreapprovalPlanID string `json:"preapproval_plan_id"`
	PayerEmail        string `json:"payer_email"`
	BackURL           string `json:"back_url"`
	ExternalReference string `json:"external_reference"`
}

type MPSubscriptionResponse struct {
	ID        string `json:"id"`
	InitPoint string `json:"init_point"`
	Status    string `json:"status"`
}

type MPWebhookPayload struct {
	Action string `json:"action"`
	Data   struct {
		ID string `json:"id"`
	} `json:"data"`
}

type MPPreapproval struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	PreapprovalPlanID string `json:"preapproval_plan_id"`
	ExternalReference string `json:"external_reference"`
	NextPaymentDate   string `json:"next_payment_date,omitempty"` // ISO-8601
}

type MPClient struct {
	accessToken string
	http        *http.Client
}

func NewMPClient(accessToken string) *MPClient {
	return &MPClient{
		accessToken: accessToken,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *MPClient) CreateSubscription(ctx context.Context, req MPSubscriptionRequest) (*MPSubscriptionResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mpBaseURL+"/preapproval", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Idempotency-Key", uuid.New().String())

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MP API error: status %d", resp.StatusCode)
	}

	var result MPSubscriptionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *MPClient) GetPreapproval(ctx context.Context, id string) (*MPPreapproval, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mpBaseURL+"/preapproval/"+id, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("MP API error: status %d", resp.StatusCode)
	}

	var result MPPreapproval
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
