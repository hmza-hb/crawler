package main

import (
	"github.com/hmza-hb/crawler/internal/platform/config"
)

var errAPIKeyRequired = config.ErrAPIKeyRequired

func loadTestConfig() (config.HTTPConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.HTTPConfig{}, err
	}
	return cfg.HTTP, nil
}

func validateTestHTTP(httpCfg config.HTTPConfig) error {
	return config.ValidateHTTP(httpCfg)
}
