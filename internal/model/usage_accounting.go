package model

import "encoding/json"

// AccountingUsage includes all observed physical attempts of this logical call.
// Missing upstream usage is unknown, never an estimate of zero actual cost.
func (r Response) AccountingUsage() Usage {
	if r.accountingUsage != nil {
		return *r.accountingUsage
	}
	return r.Usage
}

func (u *Usage) add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.CachedPromptTokens += other.CachedPromptTokens
	u.BillingUnits += other.BillingUnits
}

// Errors may retain accounting/provenance, never executable or replayable output.
func failedResponse(r Response) Response {
	return Response{ActualModel: r.ActualModel, FinishReason: r.FinishReason, Usage: r.Usage, accountingUsage: r.accountingUsage}
}

func (r chatCompletionResponse) usageResponse() Response {
	cached := r.Usage.PromptCacheHitTokens
	if cached == 0 {
		cached = r.Usage.PromptTokensDetails.CachedTokens
	}
	return Response{ActualModel: r.Model, Usage: Usage{
		PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens: r.Usage.TotalTokens, BillingUnits: r.Usage.BillingUnits, CachedPromptTokens: cached,
	}}
}

func openAIFailureUsage(raw []byte) Response {
	var decoded chatCompletionResponse
	if json.Unmarshal(raw, &decoded) != nil {
		return Response{}
	}
	return decoded.usageResponse()
}

func responsesFailureUsage(raw []byte) Response {
	var decoded responsesResponse
	if json.Unmarshal(raw, &decoded) != nil {
		return Response{}
	}
	return failedResponse(responsesFromOutput(decoded))
}
