//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd

package proxy

import "errors"

type bedrockBudgetLock struct{}

func lockBedrockBudget(string) (*bedrockBudgetLock, error) {
	return nil, errors.New("durable Bedrock budget locking unsupported on this platform")
}
func (*bedrockBudgetLock) Close() {}
