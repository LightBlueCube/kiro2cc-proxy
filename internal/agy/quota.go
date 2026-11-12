package agy

import (
	"context"
	"errors"
	"time"
)

// Account-level language server methods, verified against agy 1.5.3:
//
//	RetrieveUserQuotaSummary {} → {response:{groups:[{displayName, description,
//	    buckets:[{bucketId, displayName, window:"5h"|"weekly",
//	    remainingFraction, resetTime}]}]}}
//	GetUserStatus {} → {userStatus:{name, email, planStatus:{planInfo:{planName}},
//	    userTier:{name}}}
//
// Protobuf JSON omits zero values: an exhausted bucket has no
// remainingFraction. The fraction lags usage by more than 20 seconds.

// QuotaGroup is a set of models sharing quota buckets.
type QuotaGroup struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Buckets     []QuotaBucket `json:"buckets"`
}

type QuotaBucket struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Window string `json:"window"`
	// Remaining is the unused share, 0 to 1.
	Remaining float64   `json:"remaining"`
	ResetAt   time.Time `json:"reset_at"`
}

// UserStatus is the signed-in user and plan.
type UserStatus struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Plan  string `json:"plan"`
	Tier  string `json:"tier"`
}

// Quota reads the account's quota buckets.
func (p *Proc) Quota(ctx context.Context) ([]QuotaGroup, error) {
	var result struct {
		Response struct {
			Groups []struct {
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
				Buckets     []struct {
					BucketID          string    `json:"bucketId"`
					DisplayName       string    `json:"displayName"`
					Window            string    `json:"window"`
					RemainingFraction float64   `json:"remainingFraction"`
					ResetTime         time.Time `json:"resetTime"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"response"`
	}
	if err := p.accountCall(ctx, "RetrieveUserQuotaSummary", &result); err != nil {
		return nil, err
	}
	var groups []QuotaGroup
	for _, g := range result.Response.Groups {
		group := QuotaGroup{Name: g.DisplayName, Description: g.Description}
		for _, b := range g.Buckets {
			group.Buckets = append(group.Buckets, QuotaBucket{ID: b.BucketID, Name: b.DisplayName, Window: b.Window, Remaining: b.RemainingFraction, ResetAt: b.ResetTime})
		}
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil, errors.New("agy reported no quota groups")
	}
	return groups, nil
}

// UserStatus reads the signed-in user and plan.
func (p *Proc) UserStatus(ctx context.Context) (UserStatus, error) {
	var result struct {
		UserStatus struct {
			Name       string `json:"name"`
			Email      string `json:"email"`
			PlanStatus struct {
				PlanInfo struct {
					PlanName string `json:"planName"`
				} `json:"planInfo"`
			} `json:"planStatus"`
			UserTier struct {
				Name string `json:"name"`
			} `json:"userTier"`
		} `json:"userStatus"`
	}
	if err := p.accountCall(ctx, "GetUserStatus", &result); err != nil {
		return UserStatus{}, err
	}
	u := result.UserStatus
	return UserStatus{Email: u.Email, Name: u.Name, Plan: u.PlanStatus.PlanInfo.PlanName, Tier: u.UserTier.Name}, nil
}

// EnsureLanguageServer starts the language server when the process has
// none, by opening an empty session in spec.Cwd. It returns that session's
// id and model catalog ("" and nil when a server already ran); the caller
// closes the session when done, so under --process-per-session its worker
// does not stay behind.
func (p *Proc) EnsureLanguageServer(ctx context.Context, spec SessionSpec) (string, []ModelInfo, error) {
	ports, err := p.lsPorts()
	if err != nil || len(ports) > 0 {
		return "", nil, err
	}
	probe, models, err := p.NewSession(ctx, spec)
	if err != nil {
		return "", nil, err
	}
	deadline := time.Now().Add(lsCallTimeout)
	for time.Now().Before(deadline) {
		if ports, err := p.lsPorts(); err != nil || len(ports) > 0 {
			return probe, models, err
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return probe, models, ctx.Err()
		}
	}
	return probe, models, errNoLanguageServer
}
