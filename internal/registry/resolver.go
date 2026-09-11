package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// GGCRResolver resolves image tags to digests using go-containerregistry.
type GGCRResolver struct {
	// AllowInsecure enables fallback to HTTP when the HTTPS connection
	// to the registry fails.
	AllowInsecure bool
}

func (r *GGCRResolver) Resolve(ctx context.Context, ref string, auth []byte) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", ref, err)
	}
	if d, ok := parsed.(name.Digest); ok {
		return d.DigestStr(), nil
	}

	opts := []remote.Option{remote.WithContext(ctx)}
	if len(auth) > 0 {
		kc, err := keychainFromDockerConfig(auth)
		if err != nil {
			return "", fmt.Errorf("parsing auth for %q: %w", ref, err)
		}
		opts = append(opts, remote.WithAuthFromKeychain(kc))
	}

	desc, err := remote.Get(parsed, opts...)
	var tlsErr *tls.CertificateVerificationError
	if (errors.As(err, &tlsErr) || errors.Is(err, http.ErrSchemeMismatch)) && r.AllowInsecure {
		insecure, parseErr := name.ParseReference(ref, name.Insecure)
		if parseErr != nil {
			return "", fmt.Errorf("parsing reference %q: %w", ref, parseErr)
		}
		desc, err = remote.Get(insecure, opts...)
	}
	if err != nil {
		return "", fmt.Errorf("fetching manifest for %q: %w", ref, err)
	}
	return desc.Digest.String(), nil
}

// dockerConfigJSON mirrors the structure of a Kubernetes
// kubernetes.io/dockerconfigjson secret's .dockerconfigjson key.
type dockerConfigJSON struct {
	Auths map[string]authn.AuthConfig `json:"auths"`
}

// staticKeychain resolves credentials from a parsed dockerconfigjson.
type staticKeychain struct {
	auths map[string]authn.AuthConfig
}

func (k *staticKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	if cfg, ok := k.auths[target.RegistryStr()]; ok {
		return authn.FromConfig(cfg), nil
	}
	return authn.Anonymous, nil
}

func keychainFromDockerConfig(data []byte) (authn.Keychain, error) {
	var cfg dockerConfigJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling docker config: %w", err)
	}
	return &staticKeychain{auths: cfg.Auths}, nil
}
