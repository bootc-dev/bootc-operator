// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{name: "release", data: "bootc 1.8.0\n", want: "1.8.0"},
		{name: "prerelease", data: "bootc 1.9.0-rc.1", want: "1.9.0-rc.1"},
		{name: "empty", data: "", wantErr: true},
		{name: "missing version", data: "bootc\n", wantErr: true},
		{name: "wrong program", data: "ostree 2024.1\n", wantErr: true},
		{name: "extra fields", data: "bootc 1.8.0 (git abc123)\n", want: "1.8.0"},
		{name: "too long", data: "bootc " + strings.Repeat("1", 257), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := ParseVersion([]byte(tt.data))
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}
