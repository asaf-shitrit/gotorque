package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"
)

type OpenAIProvider struct {
	APIKey  string
	BaseURL string
	Routing Routing
	Client  *http.Client
}

func NewOpenAIProviderFromEnvironment() OpenAIProvider {
	return OpenAIProvider{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL"), Routing: RoutingFromEnvironment()}
}

func (p OpenAIProvider) ModelFor(ctx context.Context, role Role) (model.LLM, error) {
	if err := p.Routing.Validate(); err != nil {
		return nil, err
	}
	name := p.Routing[role]
	inner, err := openaimodel.NewModel(ctx, name, &openaimodel.ClientConfig{APIKey: p.APIKey, BaseURL: p.BaseURL, HTTPClient: p.Client})
	if err != nil {
		return nil, err
	}
	return NewFenceStrippingModel(inner), nil
}

// ValidateConnectivity checks credentials, endpoint reachability, and model
// IDs before a campaign starts expensive repository work. It never records the
// API key or response body in campaign state.
func (p OpenAIProvider) ValidateConnectivity(ctx context.Context) error {
	if p.APIKey == "" {
		return errors.New("OPENAI_API_KEY is required for --adk")
	}
	if err := p.Routing.Validate(); err != nil {
		return err
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("OpenAI-compatible endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("OpenAI-compatible endpoint returned HTTP %s", resp.Status)
	}
	var document struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return fmt.Errorf("decode model list: %w", err)
	}
	available := map[string]bool{}
	for _, item := range document.Data {
		available[item.ID] = true
	}
	if len(available) == 0 {
		return errors.New("OpenAI-compatible endpoint returned no models")
	}
	for _, role := range AllRoles {
		if !available[p.Routing[role]] {
			return fmt.Errorf("configured model %q for %s is not advertised by endpoint", p.Routing[role], role)
		}
	}
	return nil
}
