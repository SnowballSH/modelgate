package models

type PriceTier struct {
	AboveInputTokens int64
	Pricing          Pricing
}

func (m Model) PricingFor(promptTokens int64) Pricing {
	pricing := m.Pricing
	for _, tier := range m.PriceTiers {
		if promptTokens > tier.AboveInputTokens {
			pricing = tier.Pricing
		}
	}
	return pricing
}

func (m Model) WorstCasePricing() Pricing {
	worst := m.Pricing
	for _, tier := range m.PriceTiers {
		worst = Pricing{
			InputUSDPerMTok:      max(worst.InputUSDPerMTok, tier.Pricing.InputUSDPerMTok),
			OutputUSDPerMTok:     max(worst.OutputUSDPerMTok, tier.Pricing.OutputUSDPerMTok),
			CacheReadUSDPerMTok:  max(worst.CacheReadUSDPerMTok, tier.Pricing.CacheReadUSDPerMTok),
			CacheWriteUSDPerMTok: max(worst.CacheWriteUSDPerMTok, tier.Pricing.CacheWriteUSDPerMTok),
			USDPerSearch:         max(worst.USDPerSearch, tier.Pricing.USDPerSearch),
		}
	}
	return worst
}
