package config

import (
	"os"
)

type LogConfig struct {
	Level  string
	Format string
}

type HTTPConfig struct {
	Addr string
}

type Config struct {
	Log      LogConfig
	HTTP     HTTPConfig
	Database string
}

func Load() (*Config, error) {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	logFormat := os.Getenv("LOG_FORMAT")
	if logFormat == "" {
		logFormat = "json"
	}

	httpAddr := os.Getenv("HTTP_ADDR")
	if httpAddr == "" {
		httpAddr = ":8080"
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("UPVISTA_DATABASE_URL")
	}

	return &Config{
		Log: LogConfig{
			Level:  logLevel,
			Format: logFormat,
		},
		HTTP: HTTPConfig{
			Addr: httpAddr,
		},
		Database: dbURL,
	}, nil
}
