package credential

import "context"

// Resolver is the source of credentials. It answers "for this Target, what
// credential do you have?".
//
// Implementations:
//   - EnvResolver    — reads from environment variables (CLI)
//   - StaticResolver  — fixed credentials (config injection, testing)
//   - ChainResolver   — tries multiple Resolvers in order
//   - CachedResolver  — wraps another Resolver with caching and refresh
//
// Conventions (modeled on the AWS SDK v2 credential chain):
//   - If this Resolver cannot handle the Target, return (ResolvedCredential{}, nil).
//     This is NOT an error — it signals "try the next Resolver in the chain".
//   - If this Resolver can handle the Target but fails, return (ResolvedCredential{}, error).
//   - Callers iterating a chain check IsZero() to skip, and check error to decide
//     whether to short-circuit.
type Resolver interface {
	Resolve(ctx context.Context, target Target) (ResolvedCredential, error)
}
