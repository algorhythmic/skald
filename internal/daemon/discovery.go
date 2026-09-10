package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/algorhythmic/skald/internal/archive"
	"github.com/algorhythmic/skald/sessioncapture"
)

type DiscoveryStatus struct {
	ModifiedSince time.Time         `json:"modified_since"`
	Namespace     string            `json:"namespace"`
	Provider      string            `json:"provider"`
	Root          string            `json:"root"`
	State         string            `json:"state"`
	CheckedAt     time.Time         `json:"checked_at"`
	Candidates    int               `json:"candidates"`
	Enrolled      int               `json:"enrolled"`
	MaxSessions   int               `json:"max_sessions"`
	Rejected      int               `json:"rejected"`
	Issues        map[string]string `json:"issues"`
}

// Initialize reconciles the explicit configuration with persisted enrollment.
// Previously enrolled files stay readable after source loss or cutoff aging.
func (c *Collector) Initialize(ctx context.Context) error {
	sources := append([]archive.Registration{}, c.Config.Sources...)
	for _, root := range c.Config.Discovery {
		stored, err := c.Store.Registrations(ctx, root.Namespace)
		if err != nil {
			return err
		}
		if len(stored) > root.MaxSessions {
			return errors.New("discovery_capacity_below_enrolled_count")
		}
		for _, r := range stored {
			if r.Provider != root.Provider {
				return errors.New("namespace_provider_conflict")
			}
			if r.Root != root.Root && !slices.Contains(root.RootAliases, r.Root) {
				return errors.New("root_relocation_requires_explicit_alias")
			}
			r.Root = root.Root
			r.RootAliases = append([]string{}, root.RootAliases...)
			sources = append(sources, r)
		}
	}
	if err := c.Store.Register(ctx, sources); err != nil {
		return err
	}
	c.mu.Lock()
	c.sources = sources
	c.mu.Unlock()
	return nil
}
func (c *Collector) Sources() []archive.Registration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sources == nil {
		return append([]archive.Registration{}, c.Config.Sources...)
	}
	return append([]archive.Registration{}, c.sources...)
}
func (c *Collector) DiscoveryStatus() []DiscoveryStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []DiscoveryStatus{}
	for _, r := range c.Config.Discovery {
		s, ok := c.discovery[r.Namespace]
		if !ok {
			s = DiscoveryStatus{Namespace: r.Namespace, Provider: r.Provider, Root: r.Root, ModifiedSince: r.ModifiedSince, State: "pending", MaxSessions: r.MaxSessions}
		}
		issues := map[string]string{}
		for path, code := range s.Issues {
			issues[path] = code
		}
		s.Issues = issues
		out = append(out, s)
	}
	return out
}
func (c *Collector) discover(ctx context.Context) {
	if len(c.Config.Discovery) == 0 {
		return
	}
	now := time.Now()
	if !c.lastDiscovery.IsZero() && now.Sub(c.lastDiscovery) < 10*time.Second {
		return
	}
	c.lastDiscovery = now
	sources := c.Sources()
	for _, root := range c.Config.Discovery {
		status := DiscoveryStatus{Namespace: root.Namespace, Provider: root.Provider, Root: root.Root, ModifiedSince: root.ModifiedSince, State: "available", CheckedAt: now, MaxSessions: root.MaxSessions, Issues: map[string]string{}}
		reject := func(path, code string) {
			status.Rejected++
			if len(status.Issues) < 16 {
				status.Issues[path] = code
			}
			status.State = "partial"
		}
		known := map[string]int{}
		identities := map[string][]int{}
		for i, r := range sources {
			if r.Namespace == root.Namespace {
				known[r.Path] = i
				identities[r.ConversationID] = append(identities[r.ConversationID], i)
				status.Enrolled++
			}
		}
		candidates, err := sessioncapture.Inventory(ctx, root.Root, root.ModifiedSince, 100000)
		if err != nil {
			status.State = "unavailable"
			status.Issues["root"] = discoveryError(err)
		} else {
			status.Candidates = len(candidates)
			pending := []sessioncapture.Candidate{}
			for _, candidate := range candidates {
				if _, ok := known[candidate.Path]; !ok {
					pending = append(pending, candidate)
				}
			}
			start := 0
			if len(pending) > 0 {
				start = c.discoveryCursor[root.Namespace] % len(pending)
			}
			probes := 0
			for step := 0; step < len(pending); step++ {
				candidate := pending[(start+step)%len(pending)]
				if ctx.Err() != nil {
					return
				}
				if _, ok := known[candidate.Path]; ok {
					continue
				}
				if probes == 64 {
					if c.discoveryCursor == nil {
						c.discoveryCursor = map[string]int{}
					}
					c.discoveryCursor[root.Namespace] = (start + probes) % len(pending)
					status.State = "probe_limit"
					break
				}
				probes++
				identity, err := sessioncapture.Identify(ctx, root.Root, candidate.Path, root.Provider)
				if err != nil {
					reject(candidate.Path, discoveryError(err))
					continue
				}
				r := archive.Registration{Source: sessioncapture.Source{Namespace: root.Namespace, Provider: root.Provider, ConversationID: identity.ConversationID, ProviderVersion: identity.ProviderVersion}, Root: root.Root, Path: candidate.Path, RootAliases: root.RootAliases}
				// A moved primary file may retain its stream token only after a complete
				// captured-prefix match and proof the prior locator is absent. Competing
				// files are explicit ambiguity, not aliases inferred from conversation ID.
				replace := -1
				if prior := identities[identity.ConversationID]; len(prior) > 0 {
					if len(prior) != 1 {
						reject(candidate.Path, "ambiguous_conversation_locators")
						continue
					}
					old := sources[prior[0]]
					absent, e := locatorAbsent(old)
					if e != nil || !absent {
						reject(candidate.Path, "duplicate_conversation_locator")
						continue
					}
					cp, e := c.Store.Checkpoint(ctx, old.Key())
					if e != nil || cp.Offset == 0 {
						reject(candidate.Path, "relocation_identity_unverified")
						continue
					}
					if e := verifyRelocation(ctx, r, cp); e != nil {
						reject(candidate.Path, "relocation_prefix_mismatch")
						continue
					}
					r.StreamID = old.StreamID
					replace = prior[0]
				} else {
					if status.Enrolled >= root.MaxSessions {
						status.State = "capacity_reached"
						// Keep probing: a later candidate may relocate an enrolled
						// stream without consuming another slot.
						continue
					}
					r.StreamID = archive.NewID()
				}
				if err := c.Store.Enroll(ctx, []archive.Registration{r}); err != nil {
					reject(candidate.Path, "enrollment_failed")
					continue
				}
				if replace >= 0 {
					delete(known, sources[replace].Path)
					sources[replace] = r
					known[r.Path] = replace
				} else {
					known[r.Path] = len(sources)
					identities[r.ConversationID] = []int{len(sources)}
					sources = append(sources, r)
					status.Enrolled++
				}
			}
		}
		c.mu.Lock()
		c.sources = append([]archive.Registration{}, sources...)
		if c.discovery == nil {
			c.discovery = map[string]DiscoveryStatus{}
		}
		c.discovery[root.Namespace] = status
		c.mu.Unlock()
	}
}
func locatorAbsent(r archive.Registration) (bool, error) {
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	_, err = root.Lstat(r.Path)
	if os.IsNotExist(err) {
		return true, nil
	}
	return false, err
}
func verifyRelocation(ctx context.Context, r archive.Registration, cp sessioncapture.Checkpoint) error {
	root, err := os.OpenRoot(r.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(r.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < cp.Offset {
		return errors.New("prefix_unavailable")
	}
	h := sha256.New()
	if _, err := io.CopyN(h, cancelReader{f, ctx}, cp.Offset); err != nil {
		return err
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != cp.PrefixDigest {
		return errors.New("prefix_mismatch")
	}
	return nil
}
func discoveryError(err error) string {
	switch err.Error() {
	case "discovery_root_unavailable", "discovery_entry_unavailable", "discovery_limit_exceeded", "source_unavailable", "regular_source_required", "identity_probe_limit", "subagent_identity_unsupported", "conversation_identity_mismatch", "source_replaced_during_read", "conversation_identity_unavailable", "identity_metadata_too_large":
		return err.Error()
	}
	return "discovery_failed"
}
