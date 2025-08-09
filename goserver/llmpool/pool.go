package llmpool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Provider types
const (
	ProviderGroq      = "groq"
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
)

// Provider represents an LLM API provider
type Provider struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	APIKey   string `json:"api_key"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	Priority int    `json:"priority"` // Lower number = higher priority

	// Rate limiting
	RequestsPerMinute int       `json:"requests_per_minute"`
	RequestCount      int       `json:"-"`
	LastReset         time.Time `json:"-"`

	// Usage tracking
	TotalRequests int       `json:"-"`
	Errors        int       `json:"-"`
	LastUsed      time.Time `json:"-"`

	mu sync.Mutex `json:"-"`
}

// Structs for image + text parts
type MessagePart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *ImageURLObject `json:"image_url,omitempty"`
}

type ImageURLObject struct {
	URL string `json:"url"`
}

// Flexible ChatMessage
type ChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []MessagePart
}

// ChatRequest represents the standardized request format
type ChatRequest struct {
	Messages    []ChatMessage `json:"messages"`
	Model       string        `json:"model,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatResponse represents the standardized response format
type ChatResponse struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Model   string `json:"model"`
	Usage   struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Provider string `json:"provider"`
}

// ProviderStats contains statistics for a provider
type ProviderStats struct {
	Type              string    `json:"type"`
	Priority          int       `json:"priority"`
	RequestsPerMinute int       `json:"requests_per_minute"`
	CurrentRequests   int       `json:"current_requests"`
	TotalRequests     int       `json:"total_requests"`
	Errors            int       `json:"errors"`
	LastUsed          time.Time `json:"last_used"`
	SuccessRate       float64   `json:"success_rate"`
}

// Pool manages multiple LLM providers with load balancing and failover
type Pool struct {
	providers []*Provider
	mu        sync.RWMutex
	client    *http.Client
}

// NewPool creates a new provider pool
func NewPool() *Pool {
	return &Pool{
		providers: make([]*Provider, 0),
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewPoolWithClient creates a new provider pool with a custom HTTP client
func NewPoolWithClient(client *http.Client) *Pool {
	return &Pool{
		providers: make([]*Provider, 0),
		client:    client,
	}
}

// AddProvider adds a provider to the pool
func (p *Pool) AddProvider(provider *Provider) {
	p.mu.Lock()
	defer p.mu.Unlock()

	provider.LastReset = time.Now()
	p.providers = append(p.providers, provider)

	// Sort by priority (Groq first if same priority)
	sort.Slice(p.providers, func(i, j int) bool {
		if p.providers[i].Priority == p.providers[j].Priority {
			return p.providers[i].Type == ProviderGroq
		}
		return p.providers[i].Priority < p.providers[j].Priority
	})
}

// RemoveProvider removes a provider from the pool
func (p *Pool) RemoveProvider(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, provider := range p.providers {
		if provider.Name == name {
			p.providers = append(p.providers[:i], p.providers[i+1:]...)
			return true
		}
	}
	return false
}

// GetProviders returns a copy of all providers (without sensitive data)
func (p *Pool) GetProviders() []Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()

	providers := make([]Provider, len(p.providers))
	for i, provider := range p.providers {
		providers[i] = *provider
		providers[i].APIKey = "***" // Hide API key
	}
	return providers
}

// CanUseProvider checks if a provider can be used (rate limit check)
func (p *Pool) CanUseProvider(provider *Provider) bool {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	now := time.Now()

	// Reset rate limit counter every minute
	if now.Sub(provider.LastReset) >= time.Minute {
		provider.RequestCount = 0
		provider.LastReset = now
	}

	return provider.RequestCount < provider.RequestsPerMinute
}

// SelectProvider selects the best available provider
func (p *Pool) SelectProvider() (*Provider, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// First, try to find an available provider by priority
	for _, provider := range p.providers {
		if p.CanUseProvider(provider) {
			return provider, nil
		}
	}

	// If all providers are rate limited, return the one with the lowest usage
	if len(p.providers) == 0 {
		return nil, fmt.Errorf("no providers available")
	}

	// Return the provider that was used least recently
	leastRecent := p.providers[0]
	for _, provider := range p.providers[1:] {
		if provider.LastUsed.Before(leastRecent.LastUsed) {
			leastRecent = provider
		}
	}

	return leastRecent, nil
}

// UpdateProviderStats updates provider statistics
func (p *Pool) UpdateProviderStats(provider *Provider, success bool) {
	provider.mu.Lock()
	defer provider.mu.Unlock()

	provider.RequestCount++
	provider.TotalRequests++
	provider.LastUsed = time.Now()

	if !success {
		provider.Errors++
	}
}

// ConvertToProviderFormat converts standardized request to provider-specific format
func (p *Pool) ConvertToProviderFormat(provider *Provider, req *ChatRequest) ([]byte, error) {
	switch provider.Type {
	case ProviderGroq, ProviderOpenAI:
		// Both use OpenAI-compatible format
		openaiReq := map[string]interface{}{
			"model":       provider.Model,
			"messages":    req.Messages,
			"temperature": req.Temperature,
			"max_tokens":  req.MaxTokens,
			"stream":      req.Stream,
		}
		return json.Marshal(openaiReq)

	case ProviderAnthropic:
		// Convert to Anthropic format
		var systemMsg string
		var messages []map[string]string

		for _, msg := range req.Messages {
			if msg.Role == "system" {
				systemMsg = msg.Content.(string)
			} else {
				messages = append(messages, map[string]string{
					"role":    msg.Role,
					"content": msg.Content.(string),
				})
			}
		}

		anthropicReq := map[string]interface{}{
			"model":       provider.Model,
			"max_tokens":  req.MaxTokens,
			"temperature": req.Temperature,
			"messages":    messages,
		}

		if systemMsg != "" {
			anthropicReq["system"] = systemMsg
		}

		return json.Marshal(anthropicReq)

	default:
		return nil, fmt.Errorf("unsupported provider type: %s", provider.Type)
	}
}

// ParseProviderResponse parses provider-specific response to standardized format
func (p *Pool) ParseProviderResponse(provider *Provider, body []byte) (*ChatResponse, error) {
	var response ChatResponse
	response.Provider = provider.Name

	switch provider.Type {
	case ProviderGroq, ProviderOpenAI:
		var openaiResp struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal(body, &openaiResp); err != nil {
			return nil, err
		}

		response.ID = openaiResp.ID
		response.Model = openaiResp.Model
		response.Usage = openaiResp.Usage

		if len(openaiResp.Choices) > 0 {
			response.Content = openaiResp.Choices[0].Message.Content
		}

	case ProviderAnthropic:
		var anthropicResp struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}

		if err := json.Unmarshal(body, &anthropicResp); err != nil {
			return nil, err
		}

		response.ID = anthropicResp.ID
		response.Model = anthropicResp.Model
		response.Usage.PromptTokens = anthropicResp.Usage.InputTokens
		response.Usage.CompletionTokens = anthropicResp.Usage.OutputTokens
		response.Usage.TotalTokens = anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens

		if len(anthropicResp.Content) > 0 {
			response.Content = anthropicResp.Content[0].Text
		}

	default:
		return nil, fmt.Errorf("unsupported provider type: %s", provider.Type)
	}

	return &response, nil
}

// Chat sends a chat request using the best available provider
func (p *Pool) Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	maxRetries := len(p.providers)
	var lastErr error

	for retry := 0; retry < maxRetries; retry++ {
		provider, err := p.SelectProvider()
		if err != nil {
			return nil, err
		}

		// Convert request to provider format
		reqBody, err := p.ConvertToProviderFormat(provider, req)
		if err != nil {
			lastErr = err
			continue
		}

		// Build endpoint URL
		var endpoint string
		switch provider.Type {
		case ProviderGroq:
			endpoint = provider.BaseURL + "/chat/completions"
		case ProviderOpenAI:
			endpoint = provider.BaseURL + "/chat/completions"
		case ProviderAnthropic:
			endpoint = provider.BaseURL + "/messages"
		}

		// Create HTTP request
		httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		// Set headers
		httpReq.Header.Set("Content-Type", "application/json")
		switch provider.Type {
		case ProviderGroq, ProviderOpenAI:
			httpReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
		case ProviderAnthropic:
			httpReq.Header.Set("x-api-key", provider.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		}

		// Send request
		resp, err := p.client.Do(httpReq)
		if err != nil {
			p.UpdateProviderStats(provider, false)
			lastErr = err
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			p.UpdateProviderStats(provider, false)
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			p.UpdateProviderStats(provider, false)
			lastErr = fmt.Errorf("provider %s returned status %d: %s", provider.Name, resp.StatusCode, string(body))
			continue
		}

		// Parse response
		chatResp, err := p.ParseProviderResponse(provider, body)
		if err != nil {
			p.UpdateProviderStats(provider, false)
			lastErr = err
			continue
		}

		p.UpdateProviderStats(provider, true)
		return chatResp, nil
	}

	return nil, fmt.Errorf("all providers failed, last error: %v", lastErr)
}

// GetStats returns statistics for all providers
func (p *Pool) GetStats() map[string]ProviderStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := make(map[string]ProviderStats)

	for _, provider := range p.providers {
		provider.mu.Lock()

		successRate := 0.0
		if provider.TotalRequests > 0 {
			successRate = float64(provider.TotalRequests-provider.Errors) / float64(provider.TotalRequests) * 100
		}

		stats[provider.Name] = ProviderStats{
			Type:              provider.Type,
			Priority:          provider.Priority,
			RequestsPerMinute: provider.RequestsPerMinute,
			CurrentRequests:   provider.RequestCount,
			TotalRequests:     provider.TotalRequests,
			Errors:            provider.Errors,
			LastUsed:          provider.LastUsed,
			SuccessRate:       successRate,
		}
		provider.mu.Unlock()
	}

	return stats
}

// ProviderCount returns the number of providers in the pool
func (p *Pool) ProviderCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.providers)
}

// IsHealthy checks if the pool has at least one available provider
func (p *Pool) IsHealthy() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, provider := range p.providers {
		if p.CanUseProvider(provider) {
			return true
		}
	}
	return len(p.providers) > 0 // At least one provider exists
}
