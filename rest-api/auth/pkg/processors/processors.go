// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package processors

import (
	"github.com/NVIDIA/infra-controller/rest-api/auth/pkg/config"
	commonConfig "github.com/NVIDIA/infra-controller/rest-api/common/pkg/config"
	cdb "github.com/NVIDIA/infra-controller/rest-api/db/pkg/db"
	"github.com/rs/zerolog/log"
	temporalClient "go.temporal.io/sdk/client"
)

// NewKeycloakProcessor creates a new Keycloak token processor
func NewKeycloakProcessor(dbSession *cdb.Session, kcfg *config.KeycloakConfig) config.TokenProcessor {
	return &KeycloakProcessor{
		dbSession:      dbSession,
		keycloakConfig: kcfg,
	}
}

// NewSSAProcessor creates a new SSA token processor
func NewSSAProcessor(dbSession *cdb.Session) config.TokenProcessor {
	return &SSAProcessor{
		dbSession: dbSession,
	}
}

// NewKASProcessor creates a new KAS token processor
func NewKASProcessor(dbSession *cdb.Session, tc temporalClient.Client, encCfg *commonConfig.PayloadEncryptionConfig) config.TokenProcessor {
	return &KASProcessor{
		dbSession: dbSession,
		tc:        tc,
		encCfg:    encCfg,
	}
}

// NewCustomProcessor creates a new custom token processor
func NewCustomProcessor(dbSession *cdb.Session) config.TokenProcessor {
	return &CustomProcessor{
		dbSession: dbSession,
	}
}

// InitializeProcessors sets up all token processors in the JWTOriginConfig
func InitializeProcessors(joCfg *config.JWTOriginConfig, dbSession *cdb.Session, tc temporalClient.Client, encCfg *commonConfig.PayloadEncryptionConfig, kcfg *config.KeycloakConfig) {
	for _, origin := range []string{config.TokenOriginKeycloak, config.TokenOriginKasSsa, config.TokenOriginKasLegacy, config.TokenOriginCustom} {
		switch origin {
		case config.TokenOriginKeycloak:
			processor := NewKeycloakProcessor(dbSession, kcfg)
			joCfg.SetProcessorForOrigin(origin, processor)
		case config.TokenOriginKasSsa:
			processor := NewSSAProcessor(dbSession)
			joCfg.SetProcessorForOrigin(origin, processor)
		case config.TokenOriginKasLegacy:
			processor := NewKASProcessor(dbSession, tc, encCfg)
			joCfg.SetProcessorForOrigin(origin, processor)
		case config.TokenOriginCustom:
			processor := NewCustomProcessor(dbSession)
			joCfg.SetProcessorForOrigin(origin, processor)
		}
	}

	// The API key caches and NGC client are only allocated when a kas origin issuer
	// is configured, so an unused kas processor costs nothing
	if joCfg.GetFirstConfigByOrigin(config.TokenOriginKas) != nil {
		processor, err := NewKasOriginProcessor(dbSession)
		if err != nil {
			log.Error().Err(err).Msg("failed to initialize kas origin API key processor")
		} else {
			joCfg.SetProcessorForOrigin(config.TokenOriginKas, processor)
		}
	}
}
