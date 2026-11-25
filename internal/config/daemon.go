package config

import (
	"context"
	"fmt"
	"maps"
	"slices"

	delegationapp "github.com/arkade-os/executor/internal/core/application/delegation"
	executionapp "github.com/arkade-os/executor/internal/core/application/execution"
	"github.com/arkade-os/executor/internal/core/application/health"
	"github.com/arkade-os/executor/internal/core/application/registry"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/arkade-os/executor/internal/infrastructure/arkd"
	"github.com/arkade-os/executor/internal/infrastructure/electrum"
	"github.com/arkade-os/executor/internal/infrastructure/emulator"
	"github.com/arkade-os/executor/internal/infrastructure/postgres"
	grpcserver "github.com/arkade-os/executor/internal/interface/grpc"
	"github.com/arkade-os/executor/internal/interface/grpc/handlers"
	"github.com/arkade-os/executor/templates"
	"github.com/btcsuite/btcd/btcec/v2"
)

// Daemon is the composition root: the services wired to postgres, arkd, electrum and the emulator
// behind the gRPC server.
type Daemon struct {
	Server  *grpcserver.Server
	closers []func()
}

func NewDaemon(ctx context.Context, cfg Config) (_ *Daemon, err error) {
	d := &Daemon{}
	defer func() {
		if err != nil {
			d.Stop()
		}
	}()
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	d.closers = append(d.closers, func() { _ = db.Close() })
	ark, indexer, chain, standalone, err := d.connect(cfg)
	if err != nil {
		return nil, err
	}
	arkInfo, err := ark.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("arkd info: %w", err)
	}
	emu, emulatorKey, err := emulators(ctx, ark, arkInfo.SignerPubkey, standalone)
	if err != nil {
		return nil, err
	}
	artifacts, docs := defaultTemplates(cfg, emulatorKey != nil)

	reg := registry.NewService(db.Artifacts(), db.Templates(), registry.Limits{
		MaxArtifacts: cfg.MaxArtifacts, MaxTemplates: cfg.MaxTemplates,
		MaxDocumentBytes: cfg.MaxDocumentBytes,
	})
	if err := reg.RegisterDefaults(ctx, artifacts, docs); err != nil {
		return nil, fmt.Errorf("default templates: %w", err)
	}
	delegations := delegationapp.NewService(
		reg,
		db.Delegations(),
		db.Streams(),
		indexer,
		cfg.CosignerKey.PubKey(),
		cfg.EncryptionKey,
		arkInfo.SignerPubkey,
		emulatorKey,
		arkInfo.Network,
		delegationapp.Limits{
			MinDelegationExpiry: cfg.MinDelegationExpiry, MaxDelegations: cfg.MaxDelegations,
			MaxStreamChildren: cfg.MaxStreamChildren, MaxVariablesBytes: cfg.MaxVariablesBytes,
			ExpiryInterval: cfg.ExpiryInterval, PruneAfter: cfg.PruneAfter,
			IdleExpiry: cfg.IdleExpiry, EvictIdleAfter: cfg.EvictIdleAfter,
		},
	)
	delegations.StartStreams(ctx, docs)
	delegations.Start(context.Background())
	d.closers = append(d.closers, delegations.Stop)
	execution := executionapp.NewService(
		delegations,
		reg,
		ark,
		indexer,
		chain,
		emu,
		cfg.CosignerKey,
		db.Coins(),
		db.Attempts(),
		db.Summaries(),
		executionapp.Limits{
			OffchainConcurrency: cfg.OffchainConcurrency, AttemptTimeout: cfg.AttemptTimeout,
			RetryDelay: cfg.RetryDelay, IntentsPerBatch: cfg.IntentsPerBatch,
			IntentTimeout: cfg.IntentTimeout, Reserve: cfg.Reserve, BanDuration: cfg.BanDuration,
			LandingWindow: cfg.LandingWindow, TemplateMaxFailures: cfg.TemplateMaxFailures,
			LoopStall: cfg.LoopStall, MinBatchExpiry: cfg.MinBatchExpiry,
		},
	)
	if err := execution.Start(context.Background()); err != nil {
		return nil, fmt.Errorf("start execution: %w", err)
	}
	checker := health.NewChecker(db, ark, emu, execution)
	server, err := grpcserver.Listen(
		ctx,
		grpcserver.Options{
			PublicPort: cfg.Port, AdminPort: cfg.AdminPort, PublicRateLimit: cfg.PublicRateLimit,
			PublicRegistry: cfg.PublicRegistry, Status: execution, Health: checker,
		},
		handlers.NewExecutor(
			reg, delegations, execution, cfg.Version,
		),
		handlers.NewAdmin(reg, delegations, execution, checker),
	)
	if err != nil {
		execution.Stop()
		return nil, err
	}
	d.Server = server
	// the loop stops before the server, so nothing moves while the last requests are answered
	d.closers = append(d.closers, server.Stop, execution.Stop)
	return d, nil
}

// Stop runs the closers newest first, so the server stops before the database.
func (d *Daemon) Stop() {
	for _, v := range slices.Backward(d.closers) {
		v()
	}
	d.closers = nil
}

// connect returns the configured stand-ins or dials the URLs.
func (d *Daemon) connect(
	cfg Config,
) (ports.Signer, ports.Indexer, ports.ChainWatcher, ports.Emulator, error) {
	ark, indexer, chain, emu := cfg.Ark, cfg.Indexer, cfg.Chain, cfg.Emulator
	if ark == nil || indexer == nil {
		c, err := arkd.NewClient(cfg.ArkURL)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		d.closers = append(d.closers, c.Close)
		if ark == nil {
			ark = c
		}
		if indexer == nil {
			indexer = c
		}
	}
	if chain == nil {
		c, err := electrum.NewWatcher(cfg.ElectrumURL)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		d.closers = append(d.closers, c.Close)
		chain = c
	}
	if emu == nil && cfg.EmulatorURL != "" {
		c, err := emulator.NewClient(cfg.EmulatorURL)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		d.closers = append(d.closers, func() { _ = c.Close() })
		emu = c
	}
	return ark, indexer, chain, emu, nil
}

func emulators(
	ctx context.Context, ark ports.Signer, signerKey *btcec.PublicKey, standalone ports.Emulator,
) (ports.Emulator, *btcec.PublicKey, error) {
	arkdEmulator := arkd.NewEmulator(ark)
	if standalone == nil {
		return arkdEmulator, nil, nil
	}
	info, err := standalone.Info(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("emulator info: %w", err)
	}
	if info.SignerPubkey.IsEqual(signerKey) {
		return nil, nil, fmt.Errorf(
			"the emulator at %sEMULATOR_URL signs with arkd's signer key: unset it, arkd emulates",
			envPrefix,
		)
	}
	return emulator.NewRouter(standalone, info.SignerPubkey, arkdEmulator), info.SignerPubkey, nil
}

func defaultTemplates(cfg Config, standalone bool) (artifacts, docs map[string][]byte) {
	artifacts, docs = map[string][]byte{}, map[string][]byte{}
	if !cfg.DefaultTemplates {
		return artifacts, docs
	}
	maps.Copy(artifacts, templates.Artifacts)
	maps.Copy(docs, templates.Templates)
	if standalone {
		for name, doc := range templates.EmulatorArtifacts {
			artifacts["emulator/"+name] = doc
		}
		for name, doc := range templates.EmulatorTemplates {
			docs["emulator/"+name] = doc
		}
	}
	return artifacts, docs
}
