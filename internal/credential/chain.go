package credential

import (
	"context"
	"fmt"
	"strings"
)

// ChainResolver tries multiple Resolvers in order and returns the first
// non-zero credential.
//
// Behaviour:
//   - Each Resolver is consulted in order.
//   - If a Resolver returns a non-zero Credential, it is returned immediately.
//   - If a Resolver returns (zero, nil), the chain moves to the next Resolver.
//   - If a Resolver returns an error:
//   - When FailFast is true, the chain short-circuits and returns the error.
//   - When FailFast is false, the error is collected and the chain continues.
//   - If all Resolvers return zero, (ResolvedCredential{}, nil) is returned.
//     If FailFast is false and one or more errors were collected, they are
//     joined and returned alongside a zero credential.
type ChainResolver struct {
	Resolvers []Resolver
	FailFast  bool
}

// Resolve implements Resolver.
func (r *ChainResolver) Resolve(ctx context.Context, target Target) (ResolvedCredential, error) {
	var errs []string
	for i, res := range r.Resolvers {
		c, err := res.Resolve(ctx, target)
		if err != nil {
			if r.FailFast {
				return ResolvedCredential{}, fmt.Errorf("chain[%d] %s: %w", i, resolverName(res), err)
			}
			errs = append(errs, fmt.Sprintf("[%d] %s: %v", i, resolverName(res), err))
			continue
		}
		if !c.IsZero() {
			// Annotate source with chain position for debuggability.
			if c.Source == "" {
				c.Source = fmt.Sprintf("chain[%d]", i)
			} else {
				c.Source = fmt.Sprintf("chain[%d]=%s", i, c.Source)
			}
			return c, nil
		}
	}
	if len(errs) > 0 {
		return ResolvedCredential{}, fmt.Errorf("chain: all resolvers failed: %s", strings.Join(errs, "; "))
	}
	return ResolvedCredential{}, nil
}

// resolverName returns a short name for a Resolver, for error messages.
func resolverName(r Resolver) string {
	switch r.(type) {
	case *EnvResolver:
		return "env"
	case StaticResolver:
		return "static"
	case *ChainResolver:
		return "chain"
	case *CachedResolver:
		return "cached"
	default:
		return fmt.Sprintf("%T", r)
	}
}
