package filesystem

import (
	"fmt"
	"path"
	"sync"

	"github.com/portainer/k2d/pkg/filesystem"
)

// TODO: this package requires a lot of refactoring to make it more readable and maintainable
// configmap.go and secret.go share a lot of commonalities

// Constants representing folder names and separators used in file paths.
const (
	ConfigMapFolder       = "configmaps"
	ConfigMapSeparator    = "-k2dcm-"
	FilePathAnnotationKey = "store.k2d.io/filesystem/path"
	SecretFolder          = "secrets"
	SecretSeparator       = "-k2dsec-"

	// NamespaceNameLabelKey is the key used to store the namespace of a Configmap or Secret resource
	// in the associated metadata file
	// It is used to identify the namespace associated with a ConfigMap or a Secret
	NamespaceNameLabelKey = "store.k2d.io/filesystem/namespace-name"
)

// FileSystemStore is a structure that represents a file system store.
// It can be used to store ConfigMaps and Secrets.
// It holds paths to the configMap and secret directories,
// and a mutex to handle concurrent access.
type (
	FileSystemStore struct {
		configMapPath string
		secretPath    string
		mutex         sync.Mutex
	}
)

// FileSystemStoreOptions represents options used to create a new FileSystemStore.
type FileSystemStoreOptions struct {
	DataPath string
}

// NewFileSystemStore creates and returns a new FileSystemStore.
// It receives a data path where the directories for configMaps and secrets are created.
// If the directories cannot be created, an error is returned.
func NewFileSystemStore(opts FileSystemStoreOptions) (*FileSystemStore, error) {
	folders := []string{ConfigMapFolder, SecretFolder}

	for _, folder := range folders {
		err := filesystem.CreateDir(path.Join(opts.DataPath, folder))
		if err != nil {
			return nil, fmt.Errorf("unable to create directory %s: %w", folder, err)
		}
	}

	return &FileSystemStore{
		configMapPath: path.Join(opts.DataPath, ConfigMapFolder),
		secretPath:    path.Join(opts.DataPath, SecretFolder),
		mutex:         sync.Mutex{},
	}, nil
}
