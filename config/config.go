package config

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	Port            string
	Env             string
	InternalSecret  string
	VetclinicAPIURL string
	BillingEnabled  bool
	MPAccessToken   string
	MPWebhookSecret string
	MPPlans         map[string]string
}

func Load() *Config {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	billingEnabled, _ := strconv.ParseBool(os.Getenv("BILLING_ENABLED"))

	return &Config{
		Port:            getEnv("PORT", "8084"),
		Env:             getEnv("ENV", "development"),
		InternalSecret:  os.Getenv("INTERNAL_SECRET"),
		VetclinicAPIURL: getEnv("VETCLINIC_API_URL", "http://localhost:8080"),
		BillingEnabled:  billingEnabled,
		MPAccessToken:   os.Getenv("MP_ACCESS_TOKEN"),
		MPWebhookSecret: os.Getenv("MP_WEBHOOK_SECRET"),
		MPPlans: map[string]string{
			"basic_normal":   os.Getenv("MP_PLAN_BASIC_NORMAL"),
			"basic_beta50":   os.Getenv("MP_PLAN_BASIC_BETA50"),
			"basic_beta25":   os.Getenv("MP_PLAN_BASIC_BETA25"),
			"mid_normal":     os.Getenv("MP_PLAN_MID_NORMAL"),
			"mid_beta50":     os.Getenv("MP_PLAN_MID_BETA50"),
			"mid_beta25":     os.Getenv("MP_PLAN_MID_BETA25"),
			"premium_normal": os.Getenv("MP_PLAN_PREMIUM_NORMAL"),
			"premium_beta50": os.Getenv("MP_PLAN_PREMIUM_BETA50"),
			"premium_beta25": os.Getenv("MP_PLAN_PREMIUM_BETA25"),
		},
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
