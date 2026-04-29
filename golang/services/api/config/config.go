package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	HTTPAddr       string
	PythonGRPCAddr string
	GRPCPoolSize   int
	GRPCTimeout    time.Duration
	GinMode        string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		HTTPAddr:       getEnv("HTTP_ADDR", ":8080"),
		PythonGRPCAddr: getEnv("PYTHON_GRPC_ADDR", "localhost:50051"),
		GRPCPoolSize:   getEnvInt("GRPC_POOL_SIZE", 5),
		GRPCTimeout:    getEnvDuration("GRPC_TIMEOUT_MS", 5000) * time.Millisecond,
		GinMode:        getEnv("GIN_MODE", "debug"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallbackMs int64) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(n)
		}
	}
	return time.Duration(fallbackMs)
}
