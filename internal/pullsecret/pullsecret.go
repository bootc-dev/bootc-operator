// SPDX-License-Identifier: Apache-2.0

// Package pullsecret defines the aggregated pull secret shared by the
// controller, which writes it, and the daemon, which reads it from a
// volume mount.
package pullsecret

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// SecretName is the name of the aggregated Secret in the operator
	// namespace.
	SecretName = "bootc-pull-secrets" //nolint:gosec // G101: a name, not a credential

	// MountDir is where the daemon mounts the aggregated Secret.
	MountDir = "/var/run/bootc-pull-secrets"
)

// ErrKeyNameTooLong is returned by Key when the key name exceeds the Secret key
// length limit.
var ErrKeyNameTooLong = errors.New("key name too long")

// Key returns the aggregated Secret key for the source secret
// namespace/name. "_" cannot appear in namespace or Secret names, so the key
// is unambiguous. An error is returned if namespace or name is not a valid
// object name, since the daemon uses the key as a file name, or if the key
// exceeds the Secret key length limit, since a single invalid key makes the
// API server reject the whole aggregated Secret.
func Key(namespace, name string) (string, error) {
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return "", fmt.Errorf("pull secret %s/%s: invalid namespace: %s",
			namespace, name, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", fmt.Errorf("pull secret %s/%s: invalid name: %s",
			namespace, name, strings.Join(errs, "; "))
	}
	key := namespace + "_" + name
	if len(key) > validation.DNS1123SubdomainMaxLength {
		return "", fmt.Errorf(
			"pull secret %s/%s: %w (namespace and name must not exceed %d characters combined)",
			namespace,
			name,
			ErrKeyNameTooLong,
			validation.DNS1123SubdomainMaxLength-1,
		)
	}
	return key, nil
}

// AuthData returns the pull secret's credentials in .dockerconfigjson format,
// the one the tag resolver and bootc read, whichever of the two Kubernetes
// pull secret formats the Secret holds. It goes by the Secret's keys rather
// than its type, since an Opaque Secret works as well.
//
// .dockerconfigjson is returned as is. The legacy .dockercfg, which is the
// auths map without the wrapper (OpenShift generates such secrets for service
// accounts), is wrapped in {"auths": ...} without decoding its entries, so
// that fields like identitytoken survive; the bytes are not re-encoded. If both keys are set,
// .dockerconfigjson wins. The error names the keys but never includes
// secret data.
func AuthData(secret *corev1.Secret) ([]byte, error) {
	if data, ok := secret.Data[corev1.DockerConfigJsonKey]; ok {
		if !isJSONObject(data) {
			return nil, fmt.Errorf("%s is not a JSON object", corev1.DockerConfigJsonKey)
		}
		return data, nil
	}
	if data, ok := secret.Data[corev1.DockerConfigKey]; ok {
		if !isJSONObject(data) {
			return nil, fmt.Errorf("%s is not a JSON object", corev1.DockerConfigKey)
		}
		wrapped := append([]byte(`{"auths":`), data...)
		return append(wrapped, '}'), nil
	}
	return nil, fmt.Errorf("missing %s or %s key",
		corev1.DockerConfigJsonKey, corev1.DockerConfigKey)
}

func isJSONObject(data []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(data, &obj) == nil && obj != nil
}
