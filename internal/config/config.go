package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2"
)

const envPrefix = "EXECUTOR_"

type Config struct {
	Version             string
	DatabaseURL         string
	ArkURL              string
	EmulatorURL         string // empty without a standalone emulator
	ElectrumURL         string
	Port                int
	AdminPort           int
	MaxArtifacts        int
	MaxTemplates        int
	MaxDocumentBytes    int
	MaxDelegations      int
	MaxStreamChildren   int // active delegations the streams spawned, apart from MaxDelegations
	MaxVariablesBytes   int // variables of one client delegation, 0 for no cap
	MinDelegationExpiry time.Duration
	ExpiryInterval      time.Duration
	PruneAfter          time.Duration
	IdleExpiry          time.Duration
	EvictIdleAfter      time.Duration // at MaxDelegations, the age an unfunded one is deleted at
	OffchainConcurrency int
	AttemptTimeout      time.Duration // building and signing an offchain attempt
	RetryDelay          time.Duration // before the coins of a failed attempt are tried again
	IntentsPerBatch     int
	IntentTimeout       time.Duration // an intent not selected by then is forgotten and queued again
	MinBatchExpiry      time.Duration
	Reserve             time.Duration // before expiry, the latest a coin enters a batch
	BanDuration         time.Duration // the coins of an intent a batch failed on keep out of the queue
	LandingWindow       time.Duration // after a restart, how long a batched attempt waits to see its coins spent
	TemplateMaxFailures int           // consecutive failed attempts that disable a template
	LoopStall           time.Duration // how long the loop may stay silent past a due time before it is unhealthy
	PublicRateLimit     int           // requests per second per client address on the public port, 0 for none
	PublicRegistry      bool          // the public port registers artifacts and templates too
	LogLevel            int
	DefaultTemplates    bool
	CosignerKey         *btcec.PrivateKey
	EncryptionKey       *btcec.PrivateKey // nil refuses templates with secrets

	// Ark, Indexer, Chain and Emulator replace the clients dialled from the URLs; tests set them.
	Ark      ports.Signer
	Indexer  ports.Indexer
	Chain    ports.ChainWatcher
	Emulator ports.Emulator
}

// Default is what Load starts from before reading the environment.
func Default() Config {
	return Config{
		Port: 7080, AdminPort: 7081, MaxArtifacts: 1000, MaxTemplates: 1000,
		MaxDocumentBytes: 1 << 20, MaxDelegations: 500000, MaxStreamChildren: 10000,
		MaxVariablesBytes: 4096, MinDelegationExpiry: 24 * time.Hour, ExpiryInterval: time.Minute,
		PruneAfter: 30 * 24 * time.Hour, OffchainConcurrency: 8, AttemptTimeout: time.Minute,
		RetryDelay: time.Minute, IntentsPerBatch: 128, IntentTimeout: 5 * time.Minute,
		Reserve: 15 * time.Minute, BanDuration: 5 * time.Minute, LandingWindow: 5 * time.Minute,
		TemplateMaxFailures: 10, LoopStall: 2 * time.Minute, PublicRateLimit: 5, LogLevel: 4,
		DefaultTemplates: true, IdleExpiry: 7 * 24 * time.Hour, EvictIdleAfter: time.Hour,
		MinBatchExpiry: 512 * time.Second,
	}
}

func Load() (Config, error) {
	cfg := Default()
	cfg.DatabaseURL = os.Getenv(envPrefix + "DATABASE_URL")
	cfg.ArkURL = os.Getenv(envPrefix + "ARK_URL")
	cfg.EmulatorURL = os.Getenv(envPrefix + "EMULATOR_URL")
	cfg.ElectrumURL = os.Getenv(envPrefix + "ELECTRUM_URL")
	for name, value := range map[string]string{
		"DATABASE_URL": cfg.DatabaseURL, "ARK_URL": cfg.ArkURL, "ELECTRUM_URL": cfg.ElectrumURL,
	} {
		if value == "" {
			return Config{}, fmt.Errorf("%s%s is required", envPrefix, name)
		}
	}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"PORT", &cfg.Port},
		{"ADMIN_PORT", &cfg.AdminPort},
		{"MAX_ARTIFACTS", &cfg.MaxArtifacts},
		{"MAX_TEMPLATES", &cfg.MaxTemplates},
		{"MAX_DOCUMENT_BYTES", &cfg.MaxDocumentBytes},
		{"MAX_DELEGATIONS", &cfg.MaxDelegations},
		{"MAX_STREAM_CHILDREN", &cfg.MaxStreamChildren},
		{"MAX_VARIABLES_BYTES", &cfg.MaxVariablesBytes},
		{"OFFCHAIN_CONCURRENCY", &cfg.OffchainConcurrency},
		{"INTENTS_PER_BATCH", &cfg.IntentsPerBatch},
		{"TEMPLATE_MAX_FAILURES", &cfg.TemplateMaxFailures},
		{"PUBLIC_RATE_LIMIT", &cfg.PublicRateLimit},
		{"LOG_LEVEL", &cfg.LogLevel},
	} {
		v, err := envInt(f.name, *f.dst)
		if err != nil {
			return Config{}, err
		}
		*f.dst = v
	}
	if cfg.Port == cfg.AdminPort {
		return Config{}, fmt.Errorf("%sADMIN_PORT must differ from %sPORT", envPrefix, envPrefix)
	}
	var err error
	for _, f := range []struct {
		name string
		dst  *time.Duration
	}{
		{"MIN_DELEGATION_EXPIRY", &cfg.MinDelegationExpiry},
		{"EXPIRY_INTERVAL", &cfg.ExpiryInterval},
		{"PRUNE_AFTER", &cfg.PruneAfter},
		{"IDLE_DELEGATION_EXPIRY", &cfg.IdleExpiry},
		{"EVICT_IDLE_AFTER", &cfg.EvictIdleAfter},
		{"ATTEMPT_TIMEOUT", &cfg.AttemptTimeout},
		{"RETRY_DELAY", &cfg.RetryDelay},
		{"INTENT_TIMEOUT", &cfg.IntentTimeout},
		{"MIN_BATCH_EXPIRY", &cfg.MinBatchExpiry},
		{"RESERVE", &cfg.Reserve},
		{"BAN_DURATION", &cfg.BanDuration},
		{"LANDING_WINDOW", &cfg.LandingWindow},
		{"LOOP_STALL", &cfg.LoopStall},
	} {
		v, err := envDuration(f.name, *f.dst)
		if err != nil {
			return Config{}, err
		}
		*f.dst = v
	}
	if cfg.IntentsPerBatch < 1 {
		return Config{}, fmt.Errorf("%sINTENTS_PER_BATCH must be at least 1", envPrefix)
	}
	if cfg.TemplateMaxFailures < 1 {
		return Config{}, fmt.Errorf("%sTEMPLATE_MAX_FAILURES must be at least 1", envPrefix)
	}
	if cfg.Reserve < 2*cfg.IntentTimeout {
		return Config{}, fmt.Errorf(
			"%sRESERVE must be at least twice %sINTENT_TIMEOUT", envPrefix, envPrefix,
		)
	}
	if cfg.DefaultTemplates, err = envBool("DEFAULT_TEMPLATES", cfg.DefaultTemplates); err != nil {
		return Config{}, err
	}
	if cfg.PublicRegistry, err = envBool("PUBLIC_REGISTRY", cfg.PublicRegistry); err != nil {
		return Config{}, err
	}
	if os.Getenv(envPrefix+"COSIGNER_KEY") == "" {
		return Config{}, fmt.Errorf("%sCOSIGNER_KEY is required", envPrefix)
	}
	if cfg.CosignerKey, err = parseKey(
		"COSIGNER_KEY", os.Getenv(envPrefix+"COSIGNER_KEY"),
	); err != nil {
		return Config{}, err
	}
	if value := os.Getenv(envPrefix + "ENCRYPTION_KEY"); value != "" {
		if cfg.EncryptionKey, err = parseKey("ENCRYPTION_KEY", value); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func parseKey(env, value string) (*btcec.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%s%s must be 32 bytes hex", envPrefix, env)
	}
	var scalar btcec.ModNScalar
	if scalar.SetByteSlice(raw) || scalar.IsZero() {
		return nil, fmt.Errorf("%s%s must be a valid scalar", envPrefix, env)
	}
	return btcec.PrivKeyFromScalar(&scalar), nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(envPrefix + name)
	if raw == "" {
		return def, nil
	}
	v, err := time.ParseDuration(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s%s must be a non-negative duration", envPrefix, name)
	}
	return v, nil
}

func envInt(name string, def int) (int, error) {
	raw := os.Getenv(envPrefix + name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s%s must be a non-negative integer", envPrefix, name)
	}
	return v, nil
}

func envBool(name string, def bool) (bool, error) {
	raw := os.Getenv(envPrefix + name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s%s must be true or false", envPrefix, name)
	}
	return v, nil
}
