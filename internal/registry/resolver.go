package registry

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TagResolver resolves a container image reference to a digest.
type TagResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// GGCRResolver resolves image tags to digests using go-containerregistry.
type GGCRResolver struct {
	// AllowInsecure enables fallback to HTTP when the HTTPS connection
	// to the registry fails.
	AllowInsecure bool
}

func (r *GGCRResolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref, err)
	}
	desc, err := remote.Get(parsed, remote.WithContext(ctx))
	if err != nil && r.AllowInsecure {
		insecure, parseErr := name.ParseReference(ref, name.Insecure)
		if parseErr != nil {
			return "", fmt.Errorf("parsing reference %q: %w", ref, parseErr)
		}
		desc, err = remote.Get(insecure, remote.WithContext(ctx))
	}
	if err != nil {
		return "", fmt.Errorf("fetching manifest for %q: %w", ref, err)
	}
	return desc.Digest.String(), nil
}
