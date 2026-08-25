package main

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/luishuertagonzalez/billing-service/config"
)

func main() {
	cfg := config.Load()

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.Default()
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	log.Printf("billing-service starting on :%s (billing_enabled=%v)", cfg.Port, cfg.BillingEnabled)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
