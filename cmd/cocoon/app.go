package main

import (
	"fmt"
	"os"
	"runtime"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/hypervisor/cloudhypervisor"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/pipeline"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/storage/local"
	"github.com/CMGS/cocoon/vm"
	"github.com/CMGS/cocoon/vm/engine"
)

// appContext holds initialized managers for CLI commands.
type appContext struct {
	cfg      *config.CocoonConfig
	vmMgr    vm.Manager
	imgMgr   image.Manager // Cloud image pipeline (pull/convert/cache).
	imgBuild image.Builder // OCI VM image build/push/login.
	hyper    hypervisor.Client
	refCtr   storage.ReferenceCounter
	cowMgr   storage.COWManager
	gc       storage.GarbageCollector
}

// initApp creates and initializes all managers from CLI context.
// On Linux, it requires root (euid 0) because Cocoon requires root privileges.
func initApp(_ *cli.Context) (*appContext, error) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		return nil, fmt.Errorf("cocoon requires root privileges. Run with sudo or as root")
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// Override directories if specified via CLI flags / env vars.
	if rootDir != "" {
		cfg.RebaseRootDir(rootDir)
	}
	if runtimeDir != "" {
		cfg.RuntimeDir = runtimeDir
	}
	if logDir != "" {
		cfg.LogDir = logDir
	}

	// Ensure directories exist.
	if err := cfg.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("ensure directories: %w", err)
	}

	// Initialize managers.
	hyper := cloudhypervisor.New(cfg)
	refCtr := local.NewReferenceCounter(cfg)
	cowMgr := local.NewCOWManager(cfg)
	gc := local.NewGarbageCollector(cfg)
	imgMgr := pipeline.New(cfg, refCtr)
	imgBuild := oci.NewBuilder(cfg)
	vmMgr := engine.New(cfg, hyper, refCtr, cowMgr, imgMgr)

	return &appContext{
		cfg:      cfg,
		vmMgr:    vmMgr,
		imgMgr:   imgMgr,
		imgBuild: imgBuild,
		hyper:    hyper,
		refCtr:   refCtr,
		cowMgr:   cowMgr,
		gc:       gc,
	}, nil
}
