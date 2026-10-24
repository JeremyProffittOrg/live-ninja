// Package websearch holds the paid-search contract shared by the web tool
// service and the separately packaged phone bridge.
package websearch

// MaxCostUSD bounds one Standard-tier gpt-6-luna search: one $0.01 tool call,
// at most 128k search-context tokens plus the bounded prompt at $0.10/M input
// (including 1.25x 30-minute cache writes), and 2,048 output/reasoning tokens
// at $0.50/M. The client makes one request with no automatic retry.
// Source: https://developers.openai.com/api/docs/pricing (2026-10-22).
const MaxCostUSD = 0.03

// BudgetCapability marks a bridge that reserves the search allocation before
// forwarding it. Older bridges must not acquire a new paid path during rollout.
const BudgetCapability = "paid-web-search-v2"
