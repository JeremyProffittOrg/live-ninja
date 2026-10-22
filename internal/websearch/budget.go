// Package websearch holds the paid-search contract shared by the web tool
// service and the separately packaged phone bridge.
package websearch

// MaxCostUSD bounds one non-retried gpt-4.1-mini search: one hosted search,
// fixed 8,000 search-content input tokens, a bounded query and 600 output tokens.
// Source: https://developers.openai.com/api/docs/pricing (2026-10-22).
const MaxCostUSD = 0.02

// BudgetCapability marks a bridge that reserves the search allocation before
// forwarding it. Older bridges must not acquire a new paid path during rollout.
const BudgetCapability = "paid-web-search-v1"
