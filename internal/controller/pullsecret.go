// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ParsePullSecretNamespaces parses a comma-separated namespace list and
// prepends operatorNamespace (when non-empty), which is always allowed.
// Empty entries and surrounding whitespace are dropped and duplicates
// removed. Invalid namespace names are rejected, since the Secret informer
// for them could never sync.
func ParsePullSecretNamespaces(value, operatorNamespace string) ([]string, error) {
	var namespaces []string
	for _, ns := range append([]string{operatorNamespace}, strings.Split(value, ",")...) {
		ns = strings.TrimSpace(ns)
		if ns == "" || slices.Contains(namespaces, ns) {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("invalid namespace %q: %s", ns, strings.Join(errs, "; "))
		}
		namespaces = append(namespaces, ns)
	}
	return namespaces, nil
}
