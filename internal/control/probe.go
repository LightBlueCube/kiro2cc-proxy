package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"agyrelay/internal/conv"
	"agyrelay/internal/gateway"
	"agyrelay/internal/pool"
	"agyrelay/internal/store"
	"agyrelay/internal/usage"
)

const accountTestKeyName = "test"

// ErrBusy reports that every worker slot is taken, so a test call was not
// made and no user session was evicted.
var ErrBusy = errors.New("all worker slots are busy: try again shortly")

// AccountTestResult is the model's answer to an account test.
type AccountTestResult struct {
	Model    string
	Slug     string
	Effort   string
	Reply    string
	Duration time.Duration
}

// TestAccount asks "who are you" through the account's own agy and returns
// the reply. An empty model picks the cheapest model the account offers;
// otherwise model is a model id or slug it offers. The call is recorded
// under the test system key, so it shows on the Calls page but bills nobody.
func (c *Control) TestAccount(ctx context.Context, id, model string) (AccountTestResult, error) {
	account := c.cfg.Pool.Account(id)
	if account == nil {
		return AccountTestResult{}, store.ErrNotFound
	}

	catalog := c.cfg.Gateway.AccountModels(id)
	if len(catalog) == 0 {
		return AccountTestResult{}, &InputError{"the account has no known models: refresh it first"}
	}
	target, err := pickTestModel(catalog, model, c.Settings().Prices)
	if err != nil {
		return AccountTestResult{}, err
	}

	group := pool.QuotaGroup(target.model)
	if err := account.Available(group); err != nil {
		return AccountTestResult{}, &InputError{fmt.Sprintf("the account cannot serve %s now: %v", target.model, err)}
	}

	callCtx, cancel := context.WithTimeout(ctx, warmupCallTimeout)
	defer cancel()
	started := time.Now()
	reply, err := c.cfg.Gateway.Probe(callCtx, account, gateway.SystemCall{
		KeyHash: store.SystemKeyTest,
		KeyName: accountTestKeyName,
		Model:   target.model,
		Slug:    target.slug,
		Effort:  target.effort,
		Group:   group,
	})
	if errors.Is(err, gateway.ErrSlotsBusy) {
		return AccountTestResult{}, ErrBusy
	}
	if err != nil {
		return AccountTestResult{}, err
	}
	return AccountTestResult{Model: target.model, Slug: target.slug, Effort: target.effort, Reply: reply, Duration: time.Since(started)}, nil
}

// pickTestModel resolves model, an id or slug in catalog, to a call target;
// an empty model picks the cheapest one.
func pickTestModel(catalog []conv.ModelInfo, model string, prices []usage.Price) (warmupTarget, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return lowestEffortTarget(cheapestModel(catalog, prices)), nil
	}
	target, found := matchConfiguredModel(catalog, model)
	if !found {
		return warmupTarget{}, &InputError{fmt.Sprintf("the account does not offer %q", model)}
	}
	return target, nil
}
