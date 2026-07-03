package objectstore

import (
	"fmt"
	"os"
	"strings"
)

// S3ConfigFromEnv builds an S3Config from the UNIFIED_S3_* environment variables.
// Endpoint, bucket, key, and secret are required. UseSSL parses UNIFIED_S3_USE_SSL
// ("true"/"1" ⇒ true); Region is optional.
func S3ConfigFromEnv() (S3Config, error) {
	cfg := S3Config{
		Endpoint:        os.Getenv("UNIFIED_S3_ENDPOINT"),
		Bucket:          os.Getenv("UNIFIED_S3_BUCKET"),
		AccessKeyID:     os.Getenv("UNIFIED_S3_KEY"),
		SecretAccessKey: os.Getenv("UNIFIED_S3_SECRET"),
		Region:          os.Getenv("UNIFIED_S3_REGION"),
	}
	switch strings.ToLower(os.Getenv("UNIFIED_S3_USE_SSL")) {
	case "true", "1", "yes":
		cfg.UseSSL = true
	}
	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "UNIFIED_S3_ENDPOINT")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "UNIFIED_S3_BUCKET")
	}
	if cfg.AccessKeyID == "" {
		missing = append(missing, "UNIFIED_S3_KEY")
	}
	if cfg.SecretAccessKey == "" {
		missing = append(missing, "UNIFIED_S3_SECRET")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required S3 env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
