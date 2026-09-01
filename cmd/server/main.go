package main

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/luishuertagonzalez/billing-service/config"
	"github.com/luishuertagonzalez/billing-service/internal/billing"
)

func main() {
	cfg := config.Load()

	// Fix 6: Fail fast in non-development environments when INTERNAL_SECRET is not set.
	// An empty secret would silently disable internal auth, allowing unauthenticated access.
	if cfg.InternalSecret == "" && cfg.Env != "development" {
		log.Fatal("INTERNAL_SECRET must be set in non-development environments")
	}

	if cfg.MPWebhookSecret == "" && cfg.Env != "development" {
		log.Fatal("MP_WEBHOOK_SECRET must be set in non-development environments")
	}

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	mpClient := billing.NewMPClient(cfg.MPAccessToken)
	vetclinicClient := billing.NewVetclinicClient(cfg.VetclinicAPIURL, cfg.InternalSecret)
	billingHandler := billing.NewHandler(cfg, mpClient, vetclinicClient)

	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if cfg.BillingEnabled {
		billingHandler.RegisterRoutes(r)
	} else {
		// Fix 5: Always register /webhook as a 200 no-op even when billing is disabled.
		// Mercado Pago will retry-storm the endpoint if it gets non-2xx responses,
		// so we acknowledge silently instead of returning 503.
		r.POST("/webhook", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"received": true})
		})

		// All other billing paths return 503 when billing is disabled.
		billingGroup := r.Group("")
		billingGroup.Any("/billing/*path", func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing not enabled"})
		})
		billingGroup.Any("/internal/*path", func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing not enabled"})
		})
	}

	log.Printf("billing-service starting on :%s (billing_enabled=%v)", cfg.Port, cfg.BillingEnabled)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
