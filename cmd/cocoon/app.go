package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/vm"
)

// appContext holds initialized managers for CLI commands.
type appContext struct {
	cfg    *config.CocoonConfig
	vmMgr  vm.Manager
	imgMgr image.Manager
	hyper  hypervisor.Client
	refCtr storage.ReferenceCounter
	cowMgr storage.COWManager
	gc     storage.GarbageCollector
}

// initApp creates and initializes all managers from CLI context.
func initApp(c *cli.Context) (*appContext, error) {
	// Load config
	configPath := c.String("config")
	rootDir := c.String("root-dir")

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Override root dir if specified via CLI flag.
	if rootDir != "" {
		cfg.RootDir = rootDir
	}

	// Ensure all required directories exist.
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("create directories: %w", err)
	}

	// Initialize managers.
	hyper := hypervisor.NewClient(cfg)
	refCtr := storage.NewReferenceCounter(cfg)
	cowMgr := storage.NewCOWManager(cfg)
	gc := storage.NewGarbageCollector(cfg)
	imgMgr := image.NewManager(cfg)
	vmMgr := vm.NewManager(cfg, hyper, refCtr, cowMgr, imgMgr)

	return &appContext{
		cfg:    cfg,
		vmMgr:  vmMgr,
		imgMgr: imgMgr,
		hyper:  hyper,
		refCtr: refCtr,
		cowMgr: cowMgr,
		gc:     gc,
	}, nil
}
