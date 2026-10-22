package install

import (
	"errors"
	"fmt"
	"strconv"
)

// Options are the `spoond install` flags the phases read. The CLI fills
// them; each phase reads only its own fields (the flag names are in the
// field comments). Zero values mean the defaults below.
type Options struct {
	// HostName is the name clients reach this host by: the TLS
	// certificate name and the gateway's advertised host
	// (--host-name; spoond phase).
	HostName string
	// AdminKey is an SSH public key file for the first admin user
	// (--admin-key; spoond phase).
	AdminKey string
	// TLSCert and TLSKey are an existing certificate pair; without
	// them the spoond phase makes a self-signed pair for HostName
	// (--tls-cert, --tls-key).
	TLSCert string
	TLSKey  string

	// Hugepages is a 2 MiB page count or "auto" (--hugepages; host
	// phase). Empty means auto.
	Hugepages string
	// StoreDisk is a disk or partition the host phase turns into the
	// forkdcache ZFS pool (--store-disk). Empty means StoreDir must
	// already exist.
	StoreDisk string
	// StoreDir is the snapshot store directory (--store-dir).
	StoreDir string

	// Runtime is a local spoond-runtime tarball used instead of the
	// release download (--runtime; runtime phase).
	Runtime string

	// Images is the catalog to build: minimal, all or none
	// (--images; images phase). Empty means minimal.
	Images string
	// BaseRegistry is a registry prefix base Docker images are
	// pulled from instead of built (--base-registry; images phase).
	BaseRegistry string

	// RunnerURL and RunnerTokenFile register a Forgejo Actions
	// runner; both or neither (--runner-url, --runner-token-file).
	// The token is read from a file so it never sits in argv.
	RunnerURL       string
	RunnerTokenFile string

	// Container runs the host and spoond phases from inside the
	// Docker image, against bind mounts (--container; Docker
	// install). ConfigOnly makes the spoond phase write config and
	// secrets and start nothing (--config-only).
	Container  bool
	ConfigOnly bool
	// ArtifactsOnly makes the runtime phase install the binaries, the
	// pinned artifacts and /fc-vm and nothing else: no config, no
	// units, no command (--artifacts-only; the Docker image build).
	ArtifactsOnly bool
}

// Defaults.
const (
	DefaultStoreDir = "/forkdcache/e2b"
	HugepagesAuto   = "auto"
	ImagesMinimal   = "minimal"
	ImagesAll       = "all"
	ImagesNone      = "none"
)

// storeDir returns StoreDir or its default.
func (o Options) storeDir() string {
	if o.StoreDir != "" {
		return o.StoreDir
	}
	return DefaultStoreDir
}

// hugepages returns Hugepages or auto.
func (o Options) hugepages() string {
	if o.Hugepages != "" {
		return o.Hugepages
	}
	return HugepagesAuto
}

// images returns Images or minimal.
func (o Options) images() string {
	if o.Images != "" {
		return o.Images
	}
	return ImagesMinimal
}

// Validate checks the flag values that need no host access, so a typo
// fails before any phase runs.
func (o Options) Validate() error {
	if hp := o.hugepages(); hp != HugepagesAuto {
		n, err := strconv.Atoi(hp)
		if err != nil || n <= 0 {
			return fmt.Errorf("--hugepages %q: want a positive page count or %q", hp, HugepagesAuto)
		}
	}
	switch o.images() {
	case ImagesMinimal, ImagesAll, ImagesNone:
	default:
		return fmt.Errorf("--images %q: want %s, %s or %s", o.Images, ImagesMinimal, ImagesAll, ImagesNone)
	}
	if (o.TLSCert == "") != (o.TLSKey == "") {
		return errors.New("--tls-cert and --tls-key go together")
	}
	if (o.RunnerURL == "") != (o.RunnerTokenFile == "") {
		return errors.New("--runner-url and --runner-token-file go together")
	}
	if o.ConfigOnly && !o.Container {
		return errors.New("--config-only needs --container")
	}
	if o.ArtifactsOnly && o.Container {
		return errors.New("--artifacts-only and --container go to different stages: the image build, then the running stack")
	}
	return nil
}
