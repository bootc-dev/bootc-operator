// Package registry resolves container image tags to digests.
package registry

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

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

// Resolve returns the credentials for the target's registry. An exact key
// match wins; otherwise keys are compared after normalization, so that
// entries such as "https://index.docker.io/v1/", which kubectl and docker
// write, match the registry host like they do for the node's container
// runtime.
func (k *dockerConfigJSON) Resolve(target authn.Resource) (authn.Authenticator, error) {
	registry := target.RegistryStr()
	if cfg, ok := k.Auths[registry]; ok {
		return authn.FromConfig(cfg), nil
	}
	want := normalizeRegistry(registry)
	keys := make([]string, 0, len(k.Auths))
	for key := range k.Auths {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if normalizeAuthKey(key) == want {
			return authn.FromConfig(k.Auths[key]), nil
		}
	}
	return authn.Anonymous, nil
}

// normalizeAuthKey reduces an auths key to a registry host the way
// containers/image does for auth files: the scheme is stripped and, when
// there was one, so is the path.
func normalizeAuthKey(key string) string {
	stripped := strings.TrimPrefix(key, "http://")
	stripped = strings.TrimPrefix(stripped, "https://")
	if stripped != key {
		stripped, _, _ = strings.Cut(stripped, "/")
	}
	return normalizeRegistry(stripped)
}

// normalizeRegistry maps the Docker Hub host names to the one
// go-containerregistry uses.
func normalizeRegistry(registry string) string {
	switch registry {
	case "docker.io", "registry-1.docker.io":
		return name.DefaultRegistry
	}
	return registry
}

func keychainFromDockerConfig(data []byte) (authn.Keychain, error) {
	var cfg dockerConfigJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling docker config: %w", err)
	}
	return &cfg, nil
}
