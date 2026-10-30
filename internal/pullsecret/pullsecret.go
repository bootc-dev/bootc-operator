// SPDX-License-Identifier: Apache-2.0

// Package pullsecret defines the aggregated pull secret shared by the
// controller, which writes it, and the daemon, which reads it from a
// volume mount.
package pullsecret

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// SecretName is the name of the aggregated Secret in the operator
	// namespace.
	SecretName = "bootc-pull-secrets" //nolint:gosec // G101: a name, not a credential

	// MountDir is where the daemon mounts the aggregated Secret.
	MountDir = "/var/run/bootc-pull-secrets"
)

// ErrKeyTooLong is returned by Key when the key exceeds the Secret key length
// limit.
var ErrKeyTooLong = errors.New("name too long")

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
			ErrKeyTooLong,
			validation.DNS1123SubdomainMaxLength-1,
		)
	}
	return key, nil
}
