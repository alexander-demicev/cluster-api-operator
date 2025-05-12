package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/repository"
)

type ComponentCache struct {
	mu        sync.RWMutex
	cache     map[string]*cachedComponents
	processMu sync.Mutex
}

type cachedComponents struct {
	components repository.Components
}

var (
	componentCache     *ComponentCache
	componentCacheOnce sync.Once
)

func GetComponentCache() *ComponentCache {
	componentCacheOnce.Do(func() {
		componentCache = &ComponentCache{
			cache: make(map[string]*cachedComponents),
		}
	})
	return componentCache
}

func generateCacheKey(input repository.ComponentsInput) string {
	h := sha256.New()

	h.Write([]byte(input.Provider.Name()))
	h.Write([]byte(input.Provider.Type()))
	h.Write([]byte(input.Options.Version))

	return hex.EncodeToString(h.Sum(nil))
}

func (c *ComponentCache) Get(input repository.ComponentsInput) (repository.Components, bool) {
	key := generateCacheKey(input)

	c.mu.RLock()
	defer c.mu.RUnlock()

	if cached, exists := c.cache[key]; exists {
		return cached.components, true
	}

	return nil, false
}

func (c *ComponentCache) Put(input repository.ComponentsInput, components repository.Components) {
	key := generateCacheKey(input)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = &cachedComponents{
		components: components,
	}
}

func (c *ComponentCache) LockProcess() {
	c.processMu.Lock()
}

func (c *ComponentCache) UnlockProcess() {
	c.processMu.Unlock()
}
