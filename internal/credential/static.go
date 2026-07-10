package credential

import "context"

// StaticResolver returns pre-configured, fixed credentials. It is used for:
//   - Config injection (host-supplied secrets via secretsJSON)
//   - Testing (mock credentials)
//   - The AgentKit injection path (Host builds a StaticResolver from Keychain
//     tokens and injects it into the Runtime)
//
// StaticResolver returns credentials exactly as stored, including ExpiresAt.
// The Source field is set to "static:<target>" where target is the String()
// encoding of the Target.
type StaticResolver map[Target]Credential

// Resolve implements Resolver.
func (r StaticResolver) Resolve(_ context.Context, target Target) (ResolvedCredential, error) {
	c, ok := r[target]
	if !ok {
		return ResolvedCredential{}, nil
	}
	return ResolvedCredential{
		Credential: c,
		Source:     "static:" + target.String(),
	}, nil
}
