package llm

// TokenPricing holds per-million-token prices for a model.
type TokenPricing struct {
	InputPerMillion  float64
	OutputPerMillion float64
}

// modelPricing maps model IDs to their token pricing.
// Prices are in USD per million tokens.
var modelPricing = map[string]TokenPricing{
	// Claude 4.x / 4.5 / 4.6
	"claude-sonnet-4-5-20250929": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5-20251001":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
	"claude-opus-4-20250514":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},
	"claude-opus-4-6":            {InputPerMillion: 15.0, OutputPerMillion: 75.0},

	// Claude 3.5
	"claude-3-5-sonnet-20241022": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-3-5-haiku-20241022":  {InputPerMillion: 0.80, OutputPerMillion: 4.0},
}

// defaultPricing is used when the model is not found in the pricing map.
var defaultPricing = TokenPricing{InputPerMillion: 3.0, OutputPerMillion: 15.0}

// CalculateCost computes the estimated cost in USD for a given token usage.
func CalculateCost(model string, inputTokens, outputTokens int) float64 {
	pricing, ok := modelPricing[model]
	if !ok {
		pricing = defaultPricing
	}
	inputCost := float64(inputTokens) / 1_000_000 * pricing.InputPerMillion
	outputCost := float64(outputTokens) / 1_000_000 * pricing.OutputPerMillion
	return inputCost + outputCost
}
