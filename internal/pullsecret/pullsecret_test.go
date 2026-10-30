// SPDX-License-Identifier: Apache-2.0

package pullsecret

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestKey(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		secret    string
		want      string
		wantErr   bool
	}{
		{
			"namespace and name",
			"bootc-operator",
			"my-pull-secret",
			"bootc-operator_my-pull-secret",
			false,
		},
		{"dotted name", "team-a", "registry.example.com", "team-a_registry.example.com", false},
		{"at limit", "ns", strings.Repeat("a", 250), "ns_" + strings.Repeat("a", 250), false},
		{"over limit", "ns", strings.Repeat("a", 251), "", true},
		{"long namespace and name", strings.Repeat("n", 63), strings.Repeat("a", 253), "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := Key(tt.namespace, tt.secret)
			if tt.wantErr {
				g.Expect(err).To(MatchError(ErrKeyTooLong))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestKeyRejectsInvalidNames(t *testing.T) {
	for _, tt := range []struct{ namespace, name string }{
		{"", "secret"},
		{"ns", ""},
		{"..", "secret"},
		{"ns", "../../etc/shadow"},
		{"ns", "a/b"},
		{"ns", "a_b"},
		{"Team-A", "secret"},
	} {
		t.Run(tt.namespace+"/"+tt.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := Key(tt.namespace, tt.name)
			g.Expect(err).To(MatchError(ContainSubstring("invalid")))
		})
	}
}
