package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/luishuertagonzalez/billing-service/config"
)

type Handler struct {
	cfg             *config.Config
	mpClient        *MPClient
	vetclinicClient *VetclinicClient
}

func NewHandler(cfg *config.Config, mpClient *MPClient, vc *VetclinicClient) *Handler {
	return &Handler{cfg: cfg, mpClient: mpClient, vetclinicClient: vc}
}

func InternalAuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		got := c.GetHeader("X-Internal-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// Webhook de MP: público (MP llama directamente a este endpoint)
	r.POST("/webhook", h.Webhook)

	// Rutas internas: solo accesibles con X-Internal-Secret (llamadas por vetclinic-api)
	internal := r.Group("/", InternalAuthMiddleware(h.cfg.InternalSecret))
	internal.GET("/billing/status/:ownerUID", h.GetBillingStatus)
	internal.POST("/billing/subscribe", h.Subscribe)
	internal.POST("/billing/checkout", h.CreateCheckout)
	internal.POST("/internal/migrate-beta-discounts", h.MigrateBetaDiscounts)
}

func (h *Handler) Webhook(c *gin.Context) {
	// Read raw body before JSON parsing for HMAC verification
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read body"})
		return
	}
	// Restore body for ShouldBindJSON
	c.Request.Body = io.NopCloser(bytes.NewReader(rawBody))

	// Fix 7: Mercado Pago manifest-based HMAC-SHA256 signature verification.
	// x-signature format: "ts=<unix>,v1=<hex>"
	// Manifest: "id:{data.id};request-id:{x-request-id};ts:{ts};"
	// Skip when secret is empty (dev mode).
	if h.cfg.MPWebhookSecret != "" {
		xSig := c.GetHeader("x-signature")
		var ts, v1 string
		for _, part := range strings.Split(xSig, ",") {
			part = strings.TrimSpace(part)
			if kv := strings.SplitN(part, "=", 2); len(kv) == 2 {
				switch kv[0] {
				case "ts":
					ts = kv[1]
				case "v1":
					v1 = kv[1]
				}
			}
		}

		// Validate timestamp window to prevent replay attacks
		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || abs(time.Now().Unix()-tsInt) > 300 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp out of window"})
			return
		}

		// Parse data.id from raw body to build the manifest
		var bodyData struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		json.Unmarshal(rawBody, &bodyData) //nolint:errcheck — parse best-effort; empty ID → manifest mismatch → reject

		xRequestID := c.Request.Header.Get("x-request-id")
		manifest := fmt.Sprintf("id:%s;request-id:%s;ts:%s;", bodyData.Data.ID, xRequestID, ts)

		mac := hmac.New(sha256.New, []byte(h.cfg.MPWebhookSecret))
		mac.Write([]byte(manifest))
		expected := hex.EncodeToString(mac.Sum(nil))

		// Use constant-time comparison to prevent timing attacks
		if v1 == "" || !hmac.Equal([]byte(v1), []byte(expected)) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
	}

	var payload MPWebhookPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	log.Printf("webhook: received action=%s data.id=%s", payload.Action, payload.Data.ID)

	// Solo procesar eventos de actualización o cancelación
	if payload.Action != "updated" && payload.Action != "cancelled" {
		c.JSON(http.StatusOK, gin.H{"received": true})
		return
	}

	preapproval, err := h.mpClient.GetPreapproval(c.Request.Context(), payload.Data.ID)
	if err != nil {
		log.Printf("webhook: failed to get preapproval %s: %v", payload.Data.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch preapproval"})
		return
	}

	// ownerUID viene del external_reference que pusimos al crear la suscripción
	ownerUID := preapproval.ExternalReference
	if ownerUID == "" {
		log.Printf("webhook: preapproval %s has no external_reference, skipping", preapproval.ID)
		c.JSON(http.StatusOK, gin.H{"received": true})
		return
	}

	billingStatus := mpStatusToBillingStatus(preapproval.Status)
	fields := map[string]interface{}{
		"billing_status":         billingStatus,
		"updated_at":             time.Now(),
		"last_processed_event_id": payload.Data.ID,
	}

	// Revocar descuento beta solo en cancelación o expiración definitiva
	if preapproval.Status == "cancelled" || preapproval.Status == "expired" {
		fields["discount_tier"] = "revoked"
	}

	// Fix 8b: write current_period_end from next_payment_date
	if preapproval.NextPaymentDate != "" {
		if t, err := time.Parse(time.RFC3339, preapproval.NextPaymentDate); err == nil {
			fields["current_period_end"] = t
		}
	}

	// Dedup: skip if this event was already processed
	currentSub, err := h.vetclinicClient.GetSubscription(c.Request.Context(), ownerUID)
	if err == nil && currentSub != nil && currentSub.LastProcessedEventID == payload.Data.ID {
		log.Printf("webhook: duplicate event %s for %s, skipping", payload.Data.ID, ownerUID)
		c.JSON(http.StatusOK, gin.H{"received": true})
		return
	}

	if err := h.vetclinicClient.UpdateSubscription(c.Request.Context(), ownerUID, fields); err != nil {
		log.Printf("webhook: failed to update subscription for %s: %v", ownerUID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update subscription"})
		return
	}

	// Propagar billing_status a todas las clínicas del owner
	clinics, err := h.vetclinicClient.GetClinicsByOwnerUID(c.Request.Context(), ownerUID)
	if err != nil {
		log.Printf("webhook: failed to get clinics for %s: %v", ownerUID, err)
	} else {
		for _, clinic := range clinics {
			if err := h.vetclinicClient.UpdateClinicBillingStatus(c.Request.Context(), clinic.ID, billingStatus); err != nil {
				log.Printf("webhook: failed to update clinic %s billing_status: %v", clinic.ID, err)
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{"received": true})
}

func mpStatusToBillingStatus(mpStatus string) string {
	switch mpStatus {
	case "authorized":
		return "active"
	case "paused":
		return "suspended"
	case "cancelled":
		return "cancelled"
	case "expired":
		return "expired"
	default:
		return mpStatus
	}
}

func (h *Handler) GetBillingStatus(c *gin.Context) {
	ownerUID := c.Param("ownerUID")
	sub, err := h.vetclinicClient.GetSubscription(c.Request.Context(), ownerUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if sub == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"billing_status": sub.BillingStatus,
		"discount_tier":  sub.DiscountTier,
		"plan_id":        sub.PlanID,
	})
}

type SubscribeRequest struct {
	OwnerUID   string `json:"owner_uid" binding:"required"`
	Tier       string `json:"tier" binding:"required"`
	PayerEmail string `json:"payer_email" binding:"required"`
	BackURL    string `json:"back_url" binding:"required"`
}

func (h *Handler) Subscribe(c *gin.Context) {
	var req SubscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	sub, err := h.vetclinicClient.GetSubscription(c.Request.Context(), req.OwnerUID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load subscription"})
		return
	}

	// Determinar el plan según el tier y el discount_tier actual
	discountTier := "none"
	if sub != nil && sub.DiscountTier != "" && sub.DiscountTier != "revoked" {
		discountTier = sub.DiscountTier
	}
	planKey := req.Tier + "_" + discountTier
	planID, ok := h.cfg.MPPlans[planKey]
	if !ok || planID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plan not configured for: " + planKey})
		return
	}

	mpResp, err := h.mpClient.CreateSubscription(c.Request.Context(), MPSubscriptionRequest{
		PreapprovalPlanID: planID,
		PayerEmail:        req.PayerEmail,
		BackURL:           req.BackURL,
		ExternalReference: req.OwnerUID, // permite identificar al owner en el webhook
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create MP subscription"})
		return
	}

	// Guardar mp_subscription_id y plan_id en vetclinic-api
	if err := h.vetclinicClient.UpdateSubscription(c.Request.Context(), req.OwnerUID, map[string]interface{}{
		"mp_subscription_id": mpResp.ID,
		"plan_id":            planID,
	}); err != nil {
		log.Printf("subscribe: failed to save mp_subscription_id for %s: %v", req.OwnerUID, err)
	}

	c.JSON(http.StatusOK, gin.H{"init_point": mpResp.InitPoint})
}

type CheckoutItem struct {
	Title     string  `json:"title" binding:"required"`
	Quantity  int     `json:"quantity" binding:"required"`
	UnitPrice float64 `json:"unit_price" binding:"required"`
}

type CheckoutRequest struct {
	OwnerUID string         `json:"owner_uid" binding:"required"`
	Items    []CheckoutItem `json:"items" binding:"required"`
	BackURL  string         `json:"back_url" binding:"required"`
}

func (h *Handler) CreateCheckout(c *gin.Context) {
	var req CheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	items := make([]MPPreferenceItem, len(req.Items))
	for i, item := range req.Items {
		items[i] = MPPreferenceItem{
			Title:     item.Title,
			Quantity:  item.Quantity,
			UnitPrice: item.UnitPrice,
		}
	}

	pref, err := h.mpClient.CreatePreference(c.Request.Context(), MPPreferenceRequest{
		Items: items,
		BackURLs: MPBackURLs{
			Success: req.BackURL + "?status=success",
			Failure: req.BackURL + "?status=failure",
			Pending: req.BackURL + "?status=pending",
		},
		ExternalReference: req.OwnerUID,
	})
	if err != nil {
		log.Printf("checkout: failed to create preference for %s: %v", req.OwnerUID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create preference"})
		return
	}

	// En sandbox usamos sandbox_init_point; en producción, init_point
	initPoint := pref.InitPoint
	if h.cfg.Env != "production" {
		initPoint = pref.SandboxInitPoint
	}

	c.JSON(http.StatusOK, gin.H{
		"preference_id": pref.ID,
		"init_point":    initPoint,
	})
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// Fix 4: MigrateBetaDiscounts targets paying beta customers (billing_status == "active"),
// not the legacy "beta" status. This ensures only customers already converted to paid
// subscriptions receive the tier migration.
// TODO: Add Mercado Pago plan-swap API call here once the MP API endpoint is confirmed.
func (h *Handler) MigrateBetaDiscounts(c *gin.Context) {
	subs, err := h.vetclinicClient.QuerySubscriptions(c.Request.Context(), "beta50", "active")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	migrated := 0
	for _, sub := range subs {
		if err := h.vetclinicClient.UpdateSubscription(c.Request.Context(), sub.OwnerUID, map[string]interface{}{
			"discount_tier": "beta25",
			"updated_at":    time.Now(),
		}); err != nil {
			log.Printf("migrate: failed to update %s: %v", sub.OwnerUID, err)
			continue
		}
		migrated++
	}

	c.JSON(http.StatusOK, gin.H{"migrated": migrated})
}
