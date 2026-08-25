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
		// Register all billing paths as 503 when billing is disabled
		billingGroup := r.Group("")
		billingGroup.Any("/billing/*path", func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing not enabled"})
		})
		billingGroup.POST("/webhook", func(c *gin.Context) {
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
