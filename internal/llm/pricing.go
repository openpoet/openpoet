package llm

// TokenPricing holds per-million-token prices for a model.
type TokenPricing struct {
	InputPerMillion  float64
	OutputPerMillion float64

	// LongPromptThreshold, when > 0, switches to the Long* rates for prompts
	// with more input tokens than this (Claude Haiku 5.5 bills prompts over
	// 100K tokens on a second rate card).
	LongPromptThreshold  int
	LongInputPerMillion  float64
	LongOutputPerMillion float64
}

// modelPricing maps model IDs to their token pricing.
// Prices are in USD per million tokens (Anthropic first-party API rates).
var modelPricing = map[string]TokenPricing{
	// Claude 5.x
	"claude-fable-5-1":  {InputPerMillion: 10.0, OutputPerMillion: 50.0},
	"claude-fable-5":    {InputPerMillion: 10.0, OutputPerMillion: 50.0},
	"claude-opus-5-5":   {InputPerMillion: 4.0, OutputPerMillion: 20.0},
	"claude-opus-5":     {InputPerMillion: 5.0, OutputPerMillion: 25.0},
	"claude-sonnet-5-5": {InputPerMillion: 2.0, OutputPerMillion: 10.0},
	"claude-sonnet-5":   {InputPerMillion: 2.0, OutputPerMillion: 10.0},
	"claude-haiku-5-5": {InputPerMillion: 0.10, OutputPerMillion: 0.50,
		LongPromptThreshold: 100_000, LongInputPerMillion: 0.50, LongOutputPerMillion: 2.50},

	// Claude 4.x
	"claude-opus-4-8":            {InputPerMillion: 5.0, OutputPerMillion: 25.0},
	"claude-opus-4-7":            {InputPerMillion: 5.0, OutputPerMillion: 25.0},
	"claude-opus-4-6":            {InputPerMillion: 5.0, OutputPerMillion: 25.0},
	"claude-sonnet-4-6":          {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-sonnet-4-5-20250929": {InputPerMillion: 3.0, OutputPerMillion: 15.0},
	"claude-haiku-4-5":           {InputPerMillion: 1.0, OutputPerMillion: 5.0},
	"claude-haiku-4-5-20251001":  {InputPerMillion: 1.0, OutputPerMillion: 5.0},
	"claude-opus-4-20250514":     {InputPerMillion: 15.0, OutputPerMillion: 75.0},

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
	in, out := pricing.InputPerMillion, pricing.OutputPerMillion
	if pricing.LongPromptThreshold > 0 && inputTokens > pricing.LongPromptThreshold {
		in, out = pricing.LongInputPerMillion, pricing.LongOutputPerMillion
	}
	inputCost := float64(inputTokens) / 1_000_000 * in
	outputCost := float64(outputTokens) / 1_000_000 * out
	return inputCost + outputCost
}
