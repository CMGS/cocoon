# CLI Architecture and Command Structure

## Overview

The vibe CLI follows the proven architectural patterns from the core project, implementing a clean, interface-driven design using Go 1.25+ with flat package organization. The architecture emphasizes modularity, testability, and maintainability through the "全部包接口化" (all packages as interfaces) principle.

## Project Structure

```
vibe/
├── main.go                    # CLI entry point using urfave/cli/v2
├── go.mod                     # Go 1.25+ module definition
├── config/
│   ├── config.go             # Configuration types and loading
│   └── defaults.go           # Default configuration values
├── vm/
│   ├── vm.go                 # VM lifecycle management interface and implementation
│   ├── create.go             # VM creation logic
│   ├── lifecycle.go          # Start/stop/delete operations
│   └── list.go               # List and inspect operations
├── image/
│   ├── image.go              # ImageManager interface
│   ├── buildah.go            # Buildah implementation for OCI image handling
│   └── convert.go            # OCI to qcow2 conversion logic
├── storage/
│   ├── storage.go            # StorageManager interface
│   ├── qcow2.go              # qcow2 operations implementation
│   └── layout.go             # Storage layout and path management
├── hypervisor/
│   ├── hypervisor.go         # Hypervisor interface (engine pattern from core)
│   ├── cloudhypervisor.go   # Cloud Hypervisor implementation
│   └── factory/
│       └── factory.go        # Factory for hypervisor selection
├── types/
│   ├── vm.go                 # VM types and specifications
│   ├── image.go              # Image types
│   ├── config.go             # Configuration types
│   └── errors.go             # Error definitions
├── client/
│   ├── client.go             # Cloud Hypervisor REST API client
│   └── types.go              # API request/response types
├── utils/
│   ├── fs.go                 # Filesystem utilities
│   └── validation.go         # Input validation
└── version/
    └── version.go            # Version information
```

## Core Interfaces

### Hypervisor Interface

Following the core project's engine pattern, the hypervisor interface abstracts VM operations:

```go
package hypervisor

import (
    "context"
    "io"
    "time"

    "github.com/projecteru2/vibe/types"
)

// API defines the hypervisor interface (similar to core's engine.API)
type API interface {
    // Info returns hypervisor information
    Info(ctx context.Context) (*types.HypervisorInfo, error)

    // Ping checks hypervisor connectivity
    Ping(ctx context.Context) error

    // CloseConn closes the connection
    CloseConn() error

    // VM lifecycle operations
    VMCreate(ctx context.Context, opts *types.VMCreateOptions) (*types.VMInfo, error)
    VMStart(ctx context.Context, id string) error
    VMStop(ctx context.Context, id string, gracefulTimeout time.Duration) error
    VMDelete(ctx context.Context, id string, force bool) error
    VMPause(ctx context.Context, id string) error
    VMResume(ctx context.Context, id string) error

    // VM information
    VMInspect(ctx context.Context, id string) (*types.VMInfo, error)
    VMList(ctx context.Context) ([]*types.VMInfo, error)

    // VM resource management
    VMResize(ctx context.Context, id string, cpus int, memory int64) error

    // Console access
    VMAttach(ctx context.Context, id string) (io.ReadWriteCloser, error)
}
```

### ImageManager Interface

```go
package image

import (
    "context"
    "io"

    "github.com/projecteru2/vibe/types"
)

// Manager defines the image management interface
type Manager interface {
    // Pull downloads an OCI image from registry
    Pull(ctx context.Context, ref string) error

    // List returns available OCI images
    List(ctx context.Context, filter string) ([]*types.ImageInfo, error)

    // Inspect returns detailed image information
    Inspect(ctx context.Context, ref string) (*types.ImageInfo, error)

    // Remove deletes an OCI image
    Remove(ctx context.Context, ref string, force bool) error

    // ConvertToQcow2 converts OCI image to qcow2 format
    ConvertToQcow2(ctx context.Context, ref string, output string) (*types.Qcow2Info, error)

    // ExtractRootfs extracts the rootfs from OCI image
    ExtractRootfs(ctx context.Context, ref string) (io.ReadCloser, error)
}
```

### StorageManager Interface

```go
package storage

import (
    "context"

    "github.com/projecteru2/vibe/types"
)

// Manager defines the storage management interface
type Manager interface {
    // CreateVolume creates a new qcow2 volume
    CreateVolume(ctx context.Context, opts *types.VolumeCreateOptions) (*types.VolumeInfo, error)

    // DeleteVolume removes a qcow2 volume
    DeleteVolume(ctx context.Context, path string) error

    // ListVolumes returns all volumes for a VM
    ListVolumes(ctx context.Context, vmID string) ([]*types.VolumeInfo, error)

    // ResizeVolume resizes a qcow2 volume
    ResizeVolume(ctx context.Context, path string, size int64) error

    // GetVolumeInfo returns volume information
    GetVolumeInfo(ctx context.Context, path string) (*types.VolumeInfo, error)

    // CloneVolume creates a copy-on-write clone
    CloneVolume(ctx context.Context, source, dest string) error
}
```

## Factory Pattern

Following core's factory pattern for engine selection:

```go
package factory

import (
    "context"
    "fmt"

    "github.com/projecteru2/vibe/config"
    "github.com/projecteru2/vibe/hypervisor"
    "github.com/projecteru2/vibe/hypervisor/cloudhypervisor"
)

type factory func(ctx context.Context, config *config.Config, endpoint string) (hypervisor.API, error)

var hypervisors = map[string]factory{
    "cloud-hypervisor": cloudhypervisor.New,
    // Future: "firecracker": firecracker.New,
    // Future: "qemu": qemu.New,
}

// NewHypervisor creates a hypervisor instance based on configuration
func NewHypervisor(ctx context.Context, cfg *config.Config, hypervisorType string) (hypervisor.API, error) {
    fn, ok := hypervisors[hypervisorType]
    if !ok {
        return nil, fmt.Errorf("unsupported hypervisor type: %s", hypervisorType)
    }
    return fn(ctx, cfg, cfg.Hypervisor.Endpoint)
}
```

## CLI Commands

Using `urfave/cli/v2` (same as core project):

### Main Application Structure

```go
package main

import (
    "fmt"
    "os"

    "github.com/projecteru2/vibe/commands"
    "github.com/projecteru2/vibe/version"
    "github.com/urfave/cli/v2"
)

var configPath string

func main() {
    cli.VersionPrinter = func(c *cli.Context) {
        fmt.Print(version.String())
    }

    app := cli.NewApp()
    app.Name = version.NAME
    app.Usage = "Lightweight VM management with OCI images"
    app.Version = version.VERSION
    app.Flags = []cli.Flag{
        &cli.StringFlag{
            Name:        "config",
            Value:       "/etc/vibe/config.yaml",
            Usage:       "config file path for vibe, in yaml",
            Destination: &configPath,
            EnvVars:     []string{"VIBE_CONFIG_PATH"},
        },
    }

    app.Commands = []*cli.Command{
        commands.CreateCommand(),
        commands.StartCommand(),
        commands.StopCommand(),
        commands.DeleteCommand(),
        commands.ListCommand(),
        commands.InspectCommand(),
        commands.ImageCommand(),
        commands.ConsoleCommand(),
    }

    if err := app.Run(os.Args); err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        os.Exit(1)
    }
}
```

### Command Definitions

#### 1. Create Command

```go
func CreateCommand() *cli.Command {
    return &cli.Command{
        Name:  "create",
        Usage: "Create a new VM from an OCI image",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:     "name",
                Aliases:  []string{"n"},
                Usage:    "VM name",
                Required: true,
            },
            &cli.StringFlag{
                Name:     "image",
                Aliases:  []string{"i"},
                Usage:    "OCI image reference",
                Required: true,
            },
            &cli.IntFlag{
                Name:    "cpus",
                Aliases: []string{"c"},
                Usage:   "Number of vCPUs",
                Value:   1,
            },
            &cli.StringFlag{
                Name:    "memory",
                Aliases: []string{"m"},
                Usage:   "Memory size (e.g., 512M, 1G)",
                Value:   "512M",
            },
            &cli.StringFlag{
                Name:  "disk",
                Usage: "Disk size (e.g., 10G, 20G)",
                Value: "10G",
            },
            &cli.StringSliceFlag{
                Name:    "network",
                Aliases: []string{"net"},
                Usage:   "Network configuration",
            },
        },
        Action: createAction,
    }
}
```

#### 2. Start/Stop Commands

```go
func StartCommand() *cli.Command {
    return &cli.Command{
        Name:      "start",
        Usage:     "Start a VM",
        ArgsUsage: "<vm-id>",
        Action:    startAction,
    }
}

func StopCommand() *cli.Command {
    return &cli.Command{
        Name:      "stop",
        Usage:     "Stop a running VM",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "timeout",
                Usage: "Graceful shutdown timeout",
                Value: 30 * time.Second,
            },
            &cli.BoolFlag{
                Name:  "force",
                Usage: "Force stop VM",
            },
        },
        Action: stopAction,
    }
}
```

#### 3. List/Inspect Commands

```go
func ListCommand() *cli.Command {
    return &cli.Command{
        Name:    "list",
        Aliases: []string{"ls"},
        Usage:   "List all VMs",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "all",
                Aliases: []string{"a"},
                Usage:   "Show all VMs (including stopped)",
            },
            &cli.StringFlag{
                Name:  "format",
                Usage: "Output format (table, json, yaml)",
                Value: "table",
            },
        },
        Action: listAction,
    }
}

func InspectCommand() *cli.Command {
    return &cli.Command{
        Name:      "inspect",
        Usage:     "Display detailed VM information",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "format",
                Usage: "Output format (json, yaml)",
                Value: "json",
            },
        },
        Action: inspectAction,
    }
}
```

#### 4. Delete Command

```go
func DeleteCommand() *cli.Command {
    return &cli.Command{
        Name:      "delete",
        Aliases:   []string{"rm"},
        Usage:     "Delete a VM and cleanup storage",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "force",
                Aliases: []string{"f"},
                Usage:   "Force delete even if VM is running",
            },
            &cli.BoolFlag{
                Name:  "volumes",
                Usage: "Remove associated volumes",
                Value: true,
            },
        },
        Action: deleteAction,
    }
}
```

#### 5. Image Command (Subcommands)

```go
func ImageCommand() *cli.Command {
    return &cli.Command{
        Name:  "image",
        Usage: "Manage OCI images",
        Subcommands: []*cli.Command{
            {
                Name:      "pull",
                Usage:     "Pull an OCI image from registry",
                ArgsUsage: "<image-ref>",
                Action:    imagePullAction,
            },
            {
                Name:    "list",
                Aliases: []string{"ls"},
                Usage:   "List available OCI images",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Usage: "Output format (table, json)",
                        Value: "table",
                    },
                },
                Action: imageListAction,
            },
            {
                Name:      "inspect",
                Usage:     "Display detailed image information",
                ArgsUsage: "<image-ref>",
                Action:    imageInspectAction,
            },
            {
                Name:      "remove",
                Aliases:   []string{"rm"},
                Usage:     "Remove an OCI image",
                ArgsUsage: "<image-ref>",
                Flags: []cli.Flag{
                    &cli.BoolFlag{
                        Name:    "force",
                        Aliases: []string{"f"},
                        Usage:   "Force removal",
                    },
                },
                Action: imageRemoveAction,
            },
        },
    }
}
```

## Configuration Structure

Following core's YAML-based configuration pattern:

```go
package types

import (
    "time"
)

// Config holds vibe configuration
type Config struct {
    // Storage configuration
    Storage StorageConfig `yaml:"storage" required:"true"`

    // Hypervisor configuration
    Hypervisor HypervisorConfig `yaml:"hypervisor" required:"true"`

    // Image configuration
    Image ImageConfig `yaml:"image" required:"true"`

    // Network configuration
    Network NetworkConfig `yaml:"network"`

    // Global timeouts
    GlobalTimeout     time.Duration `yaml:"global_timeout" default:"300s"`
    ConnectionTimeout time.Duration `yaml:"connection_timeout" default:"10s"`

    // Logging
    Log LogConfig `yaml:"log"`
}

// StorageConfig defines storage settings
type StorageConfig struct {
    // Root directory for VM storage
    Root string `yaml:"root" default:"/var/lib/vibe"`

    // Images directory
    ImagesDir string `yaml:"images_dir" default:"/var/lib/vibe/images"`

    // Volumes directory
    VolumesDir string `yaml:"volumes_dir" default:"/var/lib/vibe/volumes"`

    // Default volume size
    DefaultVolumeSize string `yaml:"default_volume_size" default:"10G"`
}

// HypervisorConfig defines hypervisor settings
type HypervisorConfig struct {
    // Type of hypervisor (cloud-hypervisor, firecracker, qemu)
    Type string `yaml:"type" default:"cloud-hypervisor"`

    // Hypervisor API endpoint
    Endpoint string `yaml:"endpoint" default:"http://localhost:8080"`

    // Binary path (for process-based hypervisors)
    BinaryPath string `yaml:"binary_path" default:"/usr/local/bin/cloud-hypervisor"`

    // Socket path for communication
    SocketPath string `yaml:"socket_path" default:"/run/vibe/ch.sock"`

    // Default CPU count
    DefaultCPUs int `yaml:"default_cpus" default:"1"`

    // Default memory size
    DefaultMemory string `yaml:"default_memory" default:"512M"`
}

// ImageConfig defines image management settings
type ImageConfig struct {
    // Registry credentials
    Registries map[string]RegistryConfig `yaml:"registries"`

    // Image cache directory
    CacheDir string `yaml:"cache_dir" default:"/var/lib/vibe/cache"`

    // Buildah storage root
    BuildahRoot string `yaml:"buildah_root" default:"/var/lib/vibe/buildah"`
}

// RegistryConfig defines registry credentials
type RegistryConfig struct {
    Username string `yaml:"username"`
    Password string `yaml:"password"`
    Insecure bool   `yaml:"insecure"`
}

// NetworkConfig defines network settings
type NetworkConfig struct {
    // Default bridge name
    Bridge string `yaml:"bridge" default:"virbr0"`

    // Subnet for VM network
    Subnet string `yaml:"subnet" default:"192.168.100.0/24"`

    // DHCP range
    DHCPRange string `yaml:"dhcp_range"`
}

// LogConfig defines logging configuration
type LogConfig struct {
    Level   string `yaml:"level" default:"info"`
    UseJSON bool   `yaml:"use_json"`
    File    string `yaml:"file"`
}
```

### Example Configuration File

```yaml
# /etc/vibe/config.yaml
storage:
  root: /var/lib/vibe
  images_dir: /var/lib/vibe/images
  volumes_dir: /var/lib/vibe/volumes
  default_volume_size: 20G

hypervisor:
  type: cloud-hypervisor
  endpoint: http://localhost:8080
  binary_path: /usr/local/bin/cloud-hypervisor
  socket_path: /run/vibe/ch.sock
  default_cpus: 2
  default_memory: 1G

image:
  cache_dir: /var/lib/vibe/cache
  buildah_root: /var/lib/vibe/buildah
  registries:
    docker.io:
      username: ""
      password: ""
    ghcr.io:
      username: myuser
      password: mytoken

network:
  bridge: virbr0
  subnet: 192.168.100.0/24
  dhcp_range: 192.168.100.100-192.168.100.200

global_timeout: 300s
connection_timeout: 10s

log:
  level: info
  use_json: false
  file: /var/log/vibe.log
```

## Cloud Hypervisor REST API Integration

### Client Implementation

```go
package client

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/projecteru2/vibe/types"
)

// CloudHypervisorClient implements the Cloud Hypervisor REST API client
type CloudHypervisorClient struct {
    baseURL    string
    httpClient *http.Client
}

// NewCloudHypervisorClient creates a new Cloud Hypervisor client
func NewCloudHypervisorClient(endpoint string, timeout time.Duration) *CloudHypervisorClient {
    return &CloudHypervisorClient{
        baseURL: endpoint,
        httpClient: &http.Client{
            Timeout: timeout,
        },
    }
}

// CreateVM creates a new VM via Cloud Hypervisor API
func (c *CloudHypervisorClient) CreateVM(ctx context.Context, config *types.VMConfig) error {
    data, err := json.Marshal(config)
    if err != nil {
        return err
    }

    req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/v1/vm.create", bytes.NewReader(data))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("failed to create VM: %s", string(body))
    }

    return nil
}

// BootVM boots the VM
func (c *CloudHypervisorClient) BootVM(ctx context.Context) error {
    req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/v1/vm.boot", nil)
    if err != nil {
        return err
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("failed to boot VM: status %d", resp.StatusCode)
    }

    return nil
}

// ShutdownVM performs graceful VM shutdown
func (c *CloudHypervisorClient) ShutdownVM(ctx context.Context) error {
    req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/v1/vm.shutdown", nil)
    if err != nil {
        return err
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("failed to shutdown VM: status %d", resp.StatusCode)
    }

    return nil
}

// DeleteVM deletes the VM
func (c *CloudHypervisorClient) DeleteVM(ctx context.Context) error {
    req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/v1/vm.delete", nil)
    if err != nil {
        return err
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("failed to delete VM: status %d", resp.StatusCode)
    }

    return nil
}

// GetVMInfo retrieves VM information
func (c *CloudHypervisorClient) GetVMInfo(ctx context.Context) (*types.VMInfo, error) {
    req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/vm.info", nil)
    if err != nil {
        return nil, err
    }

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("failed to get VM info: status %d", resp.StatusCode)
    }

    var info types.VMInfo
    if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
        return nil, err
    }

    return &info, nil
}
```

## Implementation Flow

### VM Creation Flow

1. **CLI receives create command** → Parse and validate flags
2. **Load configuration** → Read YAML config from file
3. **Initialize managers** → Create hypervisor, image, storage managers via factories
4. **Check image availability** → ImageManager.List/Pull if needed
5. **Convert image** → ImageManager.ConvertToQcow2
6. **Create storage** → StorageManager.CreateVolume
7. **Configure VM** → Build Cloud Hypervisor VM configuration
8. **Create VM** → Hypervisor.VMCreate (calls CH REST API)
9. **Persist metadata** → Save VM state to local storage
10. **Return VM ID** → Display success message with VM details

### VM Start Flow

1. **Parse VM ID** → Validate input
2. **Load VM metadata** → Read VM configuration from storage
3. **Initialize hypervisor** → Connect to Cloud Hypervisor
4. **Start VM** → Hypervisor.VMStart (calls CH boot API)
5. **Wait for ready** → Poll VM status until running
6. **Update metadata** → Mark VM as running
7. **Return status** → Display VM running status

## Testing Strategy

Following core's testing patterns:

1. **Interface mocks** → Generate mocks for all interfaces using mockery
2. **Unit tests** → Test each package independently with mocked dependencies
3. **Integration tests** → Test with real Cloud Hypervisor instance
4. **CLI tests** → Test command parsing and execution flow

## Dependencies

Core dependencies (from go.mod):

```go
require (
    github.com/urfave/cli/v2 v2.27.0           // CLI framework
    gopkg.in/yaml.v3 v3.0.1                     // YAML parsing
    github.com/google/uuid v1.6.0               // UUID generation
    github.com/containers/buildah v1.35.0       // OCI image operations
    github.com/containers/image/v5 v5.30.0      // OCI image handling
    github.com/containers/storage v1.53.0       // Image storage
    // Cloud Hypervisor client - custom implementation
)
```

## Key Design Principles

1. **Interface-driven** → All major components defined as interfaces for testability
2. **Factory pattern** → Dynamic hypervisor/implementation selection
3. **Flat packages** → Avoid deep nesting, keep packages at root level
4. **Configuration-first** → YAML-based configuration with sensible defaults
5. **Error handling** → Consistent error types and wrapping
6. **Context propagation** → Pass context.Context through all operations
7. **Graceful degradation** → Handle failures gracefully with proper cleanup

## Future Extensibility

The interface-driven architecture allows easy extension:

1. **Multiple hypervisors** → Add Firecracker, QEMU implementations
2. **Storage backends** → Support different storage formats (raw, VHD)
3. **Network plugins** → Custom network configuration providers
4. **Image builders** → Alternative image conversion tools
5. **API server** → Add gRPC/REST API for remote management (like core)

This architecture closely follows the proven patterns from the core project while adapting them to the specific needs of lightweight VM management with OCI images.
