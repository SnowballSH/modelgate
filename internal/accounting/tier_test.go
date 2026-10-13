package accounting

import (
	"math"
	"testing"

	"github.com/SnowballSH/modelgate/internal/models"
	"github.com/SnowballSH/modelgate/internal/store"
)

var tieredModel = models.Model{
	Pricing: models.Pricing{InputUSDPerMTok: 1, OutputUSDPerMTok: 5, CacheReadUSDPerMTok: 0.1, CacheWriteUSDPerMTok: 2},
	PriceTiers: []models.PriceTier{{
		AboveInputTokens: 100_000,
		Pricing:          models.Pricing{InputUSDPerMTok: 5, OutputUSDPerMTok: 25, CacheReadUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 10},
	}},
}

func TestCostOnEachSideOfATierThreshold(t *testing.T) {
	tests := []struct {
		name  string
		usage store.Usage
		want  float64
	}{
		{"at the threshold", store.Usage{InputTokens: 100_000, OutputTokens: 1000},
			(100_000*1.0 + 1000*5.0) / 1e6},
		{"one token above", store.Usage{InputTokens: 100_001, OutputTokens: 1000},
			(100_001*5.0 + 1000*25.0) / 1e6},
		{"crossed only by cached tokens", store.Usage{InputTokens: 1000, CacheReadTokens: 98_000, CacheWriteTokens: 1001, OutputTokens: 1000},
			(1000*5.0 + 98_000*0.5 + 1001*10.0 + 1000*25.0) / 1e6},
	}
	for _, tc := range tests {
		got := Cost(tc.usage, tieredModel.PricingFor(tc.usage.PromptTokens()))
		if math.Abs(got-tc.want) > epsilon {
			t.Errorf("%s: cost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReservationIsPricedAtTheTopTier(t *testing.T) {
	demand := Demand{InputTokens: 1000, OutputTokens: 2000}
	got := WorstCaseCost(demand, tieredModel.WorstCasePricing())
	want := (1000*10.0 + 2000*25.0) / 1e6
	if math.Abs(got-want) > epsilon {
		t.Errorf("reservation = %v, want %v (dearest input-side rate and output rate of the top tier)", got, want)
	}
}
