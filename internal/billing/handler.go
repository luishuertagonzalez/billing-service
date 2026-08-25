package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
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
		if c.GetHeader("X-Internal-Secret") != secret {
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

	// HMAC-SHA256 signature verification (skip in dev mode when secret is empty)
	if h.cfg.MPWebhookSecret != "" {
		xSig := c.GetHeader("x-signature")
		v1 := ""
		for _, part := range strings.Split(xSig, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "v1=") {
				v1 = strings.TrimPrefix(part, "v1=")
				break
			}
		}
		mac := hmac.New(sha256.New, []byte(h.cfg.MPWebhookSecret))
		mac.Write(rawBody)
		expected := hex.EncodeToString(mac.Sum(nil))
		if v1 == "" || v1 != expected {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
	}

	var payload MPWebhookPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

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
		"billing_status": billingStatus,
		"updated_at":     time.Now(),
	}

	// Revocar descuento beta solo en cancelación o expiración definitiva
	if preapproval.Status == "cancelled" || preapproval.Status == "expired" {
		fields["discount_tier"] = "revoked"
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

func (h *Handler) MigrateBetaDiscounts(c *gin.Context) {
	subs, err := h.vetclinicClient.QuerySubscriptions(c.Request.Context(), "beta50", "beta")
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
