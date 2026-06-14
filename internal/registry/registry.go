package registry

import (
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// ToolEntry maps a prefixed tool name to its downstream target.
type ToolEntry struct {
	Tool         mcp.Tool
	OriginalName string
	Client       *client.Client
	ServerName   string
}

// ResourceEntry maps a resource URI to its downstream target.
type ResourceEntry struct {
	Resource   mcp.Resource
	Client     *client.Client
	ServerName string
}

// ResourceTemplateEntry maps a resource template URI pattern to its downstream target.
type ResourceTemplateEntry struct {
	Template   mcp.ResourceTemplate
	Client     *client.Client
	ServerName string
}

// PromptEntry maps a prefixed prompt name to its downstream target.
type PromptEntry struct {
	Prompt       mcp.Prompt
	OriginalName string
	Client       *client.Client
	ServerName   string
}

// Registry is a thread-safe store for all aggregated MCP entities.
type Registry struct {
	mu               sync.RWMutex
	tools            map[string]ToolEntry
	resources        map[string]ResourceEntry
	resourceTemplates map[string]ResourceTemplateEntry
	prompts          map[string]PromptEntry
}

func New() *Registry {
	return &Registry{
		tools:             make(map[string]ToolEntry),
		resources:         make(map[string]ResourceEntry),
		resourceTemplates: make(map[string]ResourceTemplateEntry),
		prompts:           make(map[string]PromptEntry),
	}
}

func (r *Registry) RegisterTool(name string, e ToolEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[name] = e
}

func (r *Registry) LookupTool(name string) (ToolEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.tools[name]
	return e, ok
}

func (r *Registry) RegisterResource(uri string, e ResourceEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources[uri] = e
}

func (r *Registry) LookupResource(uri string) (ResourceEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.resources[uri]
	return e, ok
}

func (r *Registry) RegisterResourceTemplate(uriTemplate string, e ResourceTemplateEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resourceTemplates[uriTemplate] = e
}

func (r *Registry) LookupResourceTemplate(uriTemplate string) (ResourceTemplateEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.resourceTemplates[uriTemplate]
	return e, ok
}

func (r *Registry) RegisterPrompt(name string, e PromptEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts[name] = e
}

func (r *Registry) LookupPrompt(name string) (PromptEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.prompts[name]
	return e, ok
}

func (r *Registry) AllTools() []ToolEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ToolEntry, 0, len(r.tools))
	for _, e := range r.tools {
		out = append(out, e)
	}
	return out
}

func (r *Registry) AllResources() []ResourceEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ResourceEntry, 0, len(r.resources))
	for _, e := range r.resources {
		out = append(out, e)
	}
	return out
}

func (r *Registry) AllResourceTemplates() []ResourceTemplateEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ResourceTemplateEntry, 0, len(r.resourceTemplates))
	for _, e := range r.resourceTemplates {
		out = append(out, e)
	}
	return out
}

func (r *Registry) AllPrompts() []PromptEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PromptEntry, 0, len(r.prompts))
	for _, e := range r.prompts {
		out = append(out, e)
	}
	return out
}

// DeleteByServer removes all registry entries that belong to the given server.
func (r *Registry) DeleteByServer(serverName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.tools {
		if e.ServerName == serverName {
			delete(r.tools, k)
		}
	}
	for k, e := range r.resources {
		if e.ServerName == serverName {
			delete(r.resources, k)
		}
	}
	for k, e := range r.resourceTemplates {
		if e.ServerName == serverName {
			delete(r.resourceTemplates, k)
		}
	}
	for k, e := range r.prompts {
		if e.ServerName == serverName {
			delete(r.prompts, k)
		}
	}
}
