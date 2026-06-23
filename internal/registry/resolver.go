package registry

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// GGCRResolver resolves image tags to digests using go-containerregistry.
type GGCRResolver struct{}

func (r *GGCRResolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref, err)
	}
	if d, ok := parsed.(name.Digest); ok {
		return d.DigestStr(), nil
	}
	desc, err := remote.Get(parsed, remote.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("fetching manifest for %q: %w", ref, err)
	}
	return desc.Digest.String(), nil
}
