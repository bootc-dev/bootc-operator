/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"

	"github.com/distribution/reference"
	. "github.com/onsi/gomega"
)

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantErr    bool
		wantDigest string
		wantTag    string
	}{
		{
			name:       "digest ref",
			ref:        testImageDigestRefA,
			wantDigest: testDigestA,
		},
		{
			name:       "digest ref with tag",
			ref:        "quay.io/example/myos:latest@" + testDigestA,
			wantDigest: testDigestA,
			wantTag:    "latest",
		},
		{
			name:    "tag ref",
			ref:     "quay.io/example/myos:latest",
			wantTag: "latest",
		},
		{
			name: "bare image",
			ref:  "quay.io/example/myos",
		},
		{
			name:    "empty string",
			ref:     "",
			wantErr: true,
		},
		{
			name:    "invalid characters",
			ref:     "INVALID:///ref",
			wantErr: true,
		},
		{
			name:    "short name",
			ref:     "myos:latest",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			ref, err := parseImageRef(tt.ref)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			if tt.wantDigest != "" {
				digested, ok := ref.(reference.Digested)
				g.Expect(ok).To(BeTrue(), "expected digest in %q", tt.ref)
				g.Expect(digested.Digest().String()).To(Equal(tt.wantDigest))
			}
			if tt.wantTag != "" {
				tagged, ok := ref.(reference.Tagged)
				g.Expect(ok).To(BeTrue(), "expected tag in %q", tt.ref)
				g.Expect(tagged.Tag()).To(Equal(tt.wantTag))
			}
		})
	}
}
