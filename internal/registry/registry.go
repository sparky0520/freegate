package registry

import (
	"sort"
	"sync"
	"time"

	"gateway/internal/config"
)

// Registry manages all buckets and aliases.
type Registry struct {
	mu      sync.RWMutex
	buckets map[string]*Bucket
	aliases map[string][]string
}

// New creates a Registry populated with buckets for each matching configured provider and model metadata.
func New(cfg *config.Config, metadata []config.ModelMetadata) *Registry {
	r := &Registry{
		buckets: make(map[string]*Bucket),
		aliases: make(map[string][]string),
	}

	// Index providers by name
	providers := make(map[string]*config.ProviderConfig)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		providers[p.Name] = p
	}

	// Create buckets for models whose provider is configured
	for _, m := range metadata {
		p, ok := providers[m.Provider]
		if !ok {
			continue
		}
		b := NewBucket(p, &m)
		r.buckets[b.Key()] = b
	}

	// Setup aliases: built-in and configured
	r.buildAliases(cfg)

	return r
}

func (r *Registry) buildAliases(cfg *config.Config) {
	// Built-in aliases:
	// "auto": all routable buckets
	// "fast": tier 1 then tier 2
	// "best-code": tier 3 then tier 2
	// "local": local models
	var allKeys, fastKeys, bestCodeKeys, localKeys []string

	for key, b := range r.buckets {
		allKeys = append(allKeys, key)
		if b.Provider.Local {
			localKeys = append(localKeys, key)
		}
		if b.Tier == 1 || b.Tier == 2 {
			fastKeys = append(fastKeys, key)
		}
		if b.Tier == 3 || b.Tier == 2 {
			bestCodeKeys = append(bestCodeKeys, key)
		}
	}

	sort.Strings(allKeys)
	sort.Strings(fastKeys)
	sort.Strings(bestCodeKeys)
	sort.Strings(localKeys)

	r.aliases["auto"] = allKeys
	r.aliases["fast"] = fastKeys
	r.aliases["best-code"] = bestCodeKeys
	r.aliases["local"] = localKeys

	// Apply user overrides / additions
	for aliasName, aCfg := range cfg.Aliases {
		if len(aCfg.Models) > 0 {
			r.aliases[aliasName] = aCfg.Models
		}
	}
}

// GetBucket retrieves a bucket by "provider/model_id".
func (r *Registry) GetBucket(key string) *Bucket {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.buckets[key]
}

// AllBuckets returns all buckets sorted by key.
func (r *Registry) AllBuckets() []*Bucket {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []*Bucket
	for _, b := range r.buckets {
		result = append(result, b)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Key() < result[j].Key()
	})
	return result
}

// GetAlias returns bucket keys assigned to an alias.
func (r *Registry) GetAlias(name string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys, ok := r.aliases[name]
	if !ok {
		return nil, false
	}
	res := make([]string, len(keys))
	copy(res, keys)
	return res, true
}

// Aliases returns a map of all alias names to their model lists.
func (r *Registry) Aliases() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string][]string)
	for k, v := range r.aliases {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// DisableProvider marks all buckets belonging to a provider as disabled.
func (r *Registry) DisableProvider(providerName, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, b := range r.buckets {
		if b.Provider.Name == providerName {
			b.Disable(reason)
		}
	}
}

// Snapshot returns snapshots of all buckets for /status.
func (r *Registry) Snapshot(now time.Time) []BucketSnapshot {
	buckets := r.AllBuckets()
	snapshots := make([]BucketSnapshot, 0, len(buckets))
	for _, b := range buckets {
		snapshots = append(snapshots, b.Snapshot(now))
	}
	return snapshots
}
