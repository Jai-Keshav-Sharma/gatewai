package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Jai-Keshav-Sharma/gatewai/internal/config"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/provider/openai"
	"github.com/Jai-Keshav-Sharma/gatewai/internal/schema"
)

// formatAdapter is the wire-format translator for a provider TYPE ("openai",
// "anthropic", "gemini"). A single adapter is shared by every INSTANCE of
// that type — two OpenAI instances with different keys reuse one adapter.
// The only thing that differs between instances is configuration (name, key,
// base URL, models), which lives on Instance, not on the adapter.
type formatAdapter interface {
	Name() string
	// BuildRequest includes baseURL because the adapter is shared and the
	// endpoint differs per instance. The public Provider interface keeps the
	// plan's exact signature; Instance injects its own baseURL here.
	BuildRequest(ctx context.Context, req *schema.UnifiedRequest, apiKey, baseURL string) (*http.Request, error)
	ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error)
	TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error)
	Models() []schema.Model
	SupportsStreaming() bool
}

// Instance is a single configured provider INSTANCE (e.g. "openai-1").
// It satisfies the Provider interface: the adapter does the format
// translation, the instance provides the identity and per-instance config.
type Instance struct {
	name       string
	apiKey     string
	baseURL    string
	timeout    time.Duration
	maxRetries int
	models     []string // model IDs this instance is allowed to serve
	adapter    formatAdapter
}

// compile-time check: Instance satisfies the Provider interface.
var _ Provider = (*Instance)(nil)

func (i *Instance) Name() string { return i.name }

// APIKey returns this instance's provider API key (Phase 1: direct from config).
func (i *Instance) APIKey() string { return i.apiKey }

// Timeout returns the per-request timeout for this instance.
func (i *Instance) Timeout() time.Duration { return i.timeout }

// MaxRetries returns the retry count for this instance (used by Phase 3 routing).
func (i *Instance) MaxRetries() int { return i.maxRetries }

func (i *Instance) BuildRequest(ctx context.Context, req *schema.UnifiedRequest, apiKey string) (*http.Request, error) {
	return i.adapter.BuildRequest(ctx, req, apiKey, i.baseURL)
}

func (i *Instance) ParseResponse(ctx context.Context, resp *http.Response) (*schema.UnifiedResponse, error) {
	return i.adapter.ParseResponse(ctx, resp)
}

func (i *Instance) TranslateStreamChunk(ctx context.Context, chunk []byte) ([]byte, error) {
	return i.adapter.TranslateStreamChunk(ctx, chunk)
}

func (i *Instance) SupportsStreaming() bool { return i.adapter.SupportsStreaming() }

// Models returns the catalog entries for exactly the models this instance
// serves (its config list), with pricing from the adapter's catalog.
func (i *Instance) Models() []schema.Model {
	byID := make(map[string]schema.Model, len(i.adapter.Models()))
	for _, m := range i.adapter.Models() {
		byID[m.ID] = m
	}
	out := make([]schema.Model, 0, len(i.models))
	for _, id := range i.models {
		m, ok := byID[id]
		if !ok {
			// Model served but not in the adapter's pricing catalog yet:
			// still list it, with zero pricing.
			m = schema.Model{ID: id, Provider: i.name}
		}
		out = append(out, m)
	}
	return out
}

// Registry holds all configured provider instances, keyed by INSTANCE name,
// plus a model → instances index for model resolution.
type Registry struct {
	instances map[string]*Instance
	byModel   map[string][]*Instance
}

// NewRegistry builds the registry from configuration. The type→adapter
// mapping is registered here — adding a provider type means registering its
// adapter in this function (Phase 2 adds anthropic and gemini).
func NewRegistry(cfg *config.Config) (*Registry, error) {
	adapters := map[string]formatAdapter{
		"openai": &openai.Adapter{},
	}
	reg := &Registry{
		instances: make(map[string]*Instance, len(cfg.Providers)),
		byModel:   make(map[string][]*Instance),
	}
	for _, pc := range cfg.Providers {
		adapter, ok := adapters[pc.Type]
		if !ok {
			return nil, fmt.Errorf("no adapter registered for provider type %q", pc.Type)
		}
		inst := &Instance{
			name:       pc.Name,
			apiKey:     string(pc.APIKey),
			baseURL:    pc.BaseURL,
			timeout:    time.Duration(pc.Timeout),
			maxRetries: pc.MaxRetries,
			models:     pc.Models,
			adapter:    adapter,
		}
		reg.instances[inst.name] = inst
		for _, m := range pc.Models {
			reg.byModel[m] = append(reg.byModel[m], inst)
		}
	}
	return reg, nil
}

// Get returns the instance with the given name.
func (r *Registry) Get(name string) (*Instance, bool) {
	inst, ok := r.instances[name]
	return inst, ok
}

// Instances returns all configured instances in config order.
func (r *Registry) Instances() []*Instance {
	out := make([]*Instance, 0, len(r.instances))
	for _, inst := range r.instances {
		out = append(out, inst)
	}
	return out
}

// Resolve returns an instance that serves the requested model.
// Phase 1: returns the FIRST configured instance serving the model.
// Phase 3 replaces this with the full router (strategies + failover).
func (r *Registry) Resolve(model string) (*Instance, bool) {
	candidates := r.byModel[model]
	if len(candidates) == 0 {
		return nil, false
	}
	return candidates[0], true
}
