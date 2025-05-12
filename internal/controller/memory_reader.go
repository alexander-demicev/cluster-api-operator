package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/pkg/errors"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	configclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client/config"
	"sigs.k8s.io/yaml"
)

// configProvider mirrors config.Provider interface and allows serialization of the corresponding info.
type configProvider struct {
	Name string                    `json:"name,omitempty"`
	URL  string                    `json:"url,omitempty"`
	Type clusterctlv1.ProviderType `json:"type,omitempty"`
}

// imageMeta allows to define transformations to apply to the image contained in the YAML manifests.
type imageMeta struct {
	Repository string `json:"repository,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

// Global cache for marshaled providers to persist across reconciliation loops
var (
	providerCacheMutex sync.RWMutex
	providerCache      = make(map[string][]byte)
	emptyImagesYAML    []byte
)

func init() {
	emptyImagesYAML, _ = yaml.Marshal(map[string]imageMeta{})
}

// MemoryReader provides a reader implementation backed by a map.
type MemoryReader struct {
	variables map[string]string
	providers []configProvider
	cacheKey  string
}

var _ configclient.Reader = &MemoryReader{}

// NewMemoryReader returns a new MemoryReader.
func NewMemoryReader() *MemoryReader {
	return &MemoryReader{
		variables: map[string]string{
			"images": string(emptyImagesYAML),
		},
		providers: []configProvider{},
		cacheKey:  "",
	}
}

// Init initializes the reader.
func (f *MemoryReader) Init(_ context.Context, _ string) error {
	return nil
}

// Get retrieves a value for the given key.
func (f *MemoryReader) Get(key string) (string, error) {
	if key == "providers" {
		marshaledData, err := f.getMarshaledProviders()
		if err != nil {
			return "", err
		}
		return string(marshaledData), nil
	}

	if val, ok := f.variables[key]; ok {
		return val, nil
	}
	return "", errors.Errorf("value for variable %q is not set", key)
}

func (f *MemoryReader) computeCacheKey() string {
	if len(f.providers) == 0 {
		return "empty"
	}

	data, err := yaml.Marshal(f.providers)
	if err != nil {
		fmt.Println("error marshaling providers:", err)
		return "error"
	}

	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (f *MemoryReader) getMarshaledProviders() ([]byte, error) {
	if f.cacheKey == "" {
		f.cacheKey = f.computeCacheKey()
	}

	providerCacheMutex.RLock()
	cachedData, found := providerCache[f.cacheKey]
	providerCacheMutex.RUnlock()

	if found {
		return cachedData, nil
	}

	data, err := yaml.Marshal(f.providers)
	if err != nil {
		return nil, err
	}

	providerCacheMutex.Lock()
	providerCache[f.cacheKey] = data
	providerCacheMutex.Unlock()

	return data, nil
}

// Set sets a value for the given key.
func (f *MemoryReader) Set(key, value string) {
	if key == "providers" {
		return
	}
	f.variables[key] = value
}

// UnmarshalKey gets a value for the given key, then unmarshals it.
func (f *MemoryReader) UnmarshalKey(key string, rawval interface{}) error {
	data, err := f.Get(key)
	if err != nil {
		return nil //nolint:nilerr // We expect to not error if the key is not present
	}
	return yaml.Unmarshal([]byte(data), rawval)
}

// AddProvider adds the given provider to the "providers" map entry and returns any errors.
func (f *MemoryReader) AddProvider(name string, ttype clusterctlv1.ProviderType, url string) (*MemoryReader, error) {
	providerCacheMutex.Lock()
	defer providerCacheMutex.Unlock()

	for _, p := range f.providers {
		if p.Name == name && p.Type == ttype && p.URL == url {
			return f, nil
		}
	}

	f.providers = append(f.providers, configProvider{
		Name: name,
		URL:  url,
		Type: ttype,
	})

	f.cacheKey = ""
	return f, nil
}
